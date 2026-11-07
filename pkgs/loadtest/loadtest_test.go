package loadtest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	platform "github.com/hungpham/loadtest-platform"
	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/runner"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// fakeRunner records calls without running k6.
type fakeRunner struct {
	provisioned []string
	started     []string
	stopped     []string
	states      map[string]string
}

// Mode reports local so the runner_type assertion holds without real k6.
func (f *fakeRunner) Mode() string { return "local" }

func (f *fakeRunner) Provision(_ context.Context, _ runner.Spec) (string, error) {
	ref := "fake-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	f.provisioned = append(f.provisioned, ref)
	f.states = map[string]string{}
	return ref, nil
}
func (f *fakeRunner) Start(_ context.Context, ref string, _ runner.Spec) error {
	f.started = append(f.started, ref)
	return nil
}
func (f *fakeRunner) Stop(_ context.Context, ref string) error {
	f.stopped = append(f.stopped, ref)
	return nil
}
func (f *fakeRunner) Status(_ context.Context, ref string) (runner.Status, error) {
	return runner.Status{State: f.states[ref]}, nil
}

func testDeps(t *testing.T) (Deps, *fakeRunner) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://ltp:ltp@localhost:5432/ltp?sslmode=disable"
	}
	db, err := store.New(context.Background(), dsn)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI requires postgres: %v", err)
		}
		t.Skipf("postgres unavailable: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := store.Migrate(context.Background(), dsn, platform.Migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fr := &fakeRunner{}
	return Deps{DB: db, Runner: fr, Now: time.Now}, fr
}

// readyFixture: tenant+user+READY namespace+VALIDATED script+service with targets.
func readyFixture(t *testing.T, d Deps) (map[string]string, string) {
	t.Helper()
	suffix := time.Now().UnixNano()
	tenant := store.Tenant{Name: "L", Slug: "lt" + itoa(suffix), Status: "ACTIVE"}
	if err := d.DB.Create(&tenant).Error; err != nil {
		t.Fatal(err)
	}
	user := store.User{TenantID: tenant.ID, Email: "lt" + itoa(suffix) + "@x.co", DisplayName: "L"}
	if err := d.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	ns := store.Namespace{
		TenantID: tenant.ID, OwnerUserID: user.ID, Name: "ns" + itoa(suffix),
		Status: "READY", ReadyForLoadtest: true, Version: 1, CleanupPolicy: []byte("{}"),
	}
	if err := d.DB.Create(&ns).Error; err != nil {
		t.Fatal(err)
	}
	sc := store.Script{
		NamespaceID: ns.ID, Name: "s" + itoa(suffix), ContentRef: ns.ID + "/s.js",
		ContentHash: "abc", SizeBytes: 10, Status: "VALIDATED", Version: 1,
	}
	if err := d.DB.Create(&sc).Error; err != nil {
		t.Fatal(err)
	}
	svc := store.Service{
		NamespaceID: ns.ID, Name: "svc", OpenAPISpec: []byte(`{}`), SpecHash: strPtr("h"),
		ParsedSummary: jsonBytes(map[string]any{"targets": []map[string]any{
			{"id": "getWidgets", "method": "GET", "path": "/widgets"},
		}}), Version: 1,
	}
	if err := d.DB.Create(&svc).Error; err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"X-Tenant-Id": tenant.ID, "X-User-Id": user.ID, "X-User-Role": "user", "X-Script-Id": sc.ID,
	}, ns.ID
}

func itoa(n int64) string {
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func doLT(t *testing.T, d Deps, h map[string]string, method, nsID, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	parent := chi.NewRouter()
	parent.Mount("/namespaces/{id}/loadtests", Router(d))
	rel := "/namespaces/" + nsID + "/loadtests" + path
	req := httptest.NewRequest(method, rel, bytesReader(body))
	for k, v := range h {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	authz.RequirePrincipal(parent).ServeHTTP(rec, req)
	return rec
}

func bytesReader(v any) *bytesReaderT {
	b, _ := json.Marshal(v)
	return &bytesReaderT{b}
}

type bytesReaderT struct{ b []byte }

func (br *bytesReaderT) Read(p []byte) (int, error) {
	if len(br.b) == 0 {
		return 0, errEOF
	}
	n := copy(p, br.b)
	br.b = br.b[n:]
	return n, nil
}

var errEOF = errEOFType{}

type errEOFType struct{}

func (errEOFType) Error() string { return "EOF" }

func TestLoadTestGatesAndLifecycle(t *testing.T) {
	d, fr := testDeps(t)
	h, nsID := readyFixture(t, d)

	// missing Idempotency-Key → 422
	rec := doLT(t, d, h, http.MethodPost, nsID, "", map[string]any{"name": "lt1", "script_id": h["X-Script-Id"]})
	if rec.Code != 422 {
		t.Fatalf("missing key: %d", rec.Code)
	}

	// create → PROVISIONING
	rec = doLT(t, d, withKey(h, "k1"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt1", "script_id": h["X-Script-Id"]})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var lt store.LoadTest
	_ = json.Unmarshal(rec.Body.Bytes(), &lt)
	if lt.Status != "PROVISIONING" || lt.RunnerType != "local" {
		t.Errorf("created = %+v", lt)
	}
	if len(fr.provisioned) != 1 {
		t.Errorf("provisioned = %v", fr.provisioned)
	}
	// targets non-empty
	var targets []map[string]any
	if json.Unmarshal(lt.Targets, &targets) != nil || len(targets) == 0 {
		t.Errorf("targets = %s", lt.Targets)
	}

	// gate: script not VALIDATED → 409
	var badScript store.Script
	_ = d.DB.First(&badScript, "id = ?", h["X-Script-Id"]).Error
	_ = d.DB.Model(&store.Script{}).Where("id = ?", badScript.ID).Update("status", "INVALID").Error
	rec = doLT(t, d, withKey(h, "k2"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt2", "script_id": badScript.ID})
	if rec.Code != 409 || !jsonContains(rec.Body.String(), "SCRIPT_NOT_VALIDATED") {
		t.Errorf("invalid script gate = (%d, %s)", rec.Code, rec.Body.String())
	}
	_ = d.DB.Model(&store.Script{}).Where("id = ?", badScript.ID).Update("status", "VALIDATED").Error

	// gate: namespace not READY → 409
	_ = d.DB.Model(&store.Namespace{}).Where("id = ?", nsID).
		Updates(map[string]any{"ready_for_loadtest": false, "status": "DEGRADED"}).Error
	rec = doLT(t, d, withKey(h, "k3"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt3", "script_id": h["X-Script-Id"]})
	if rec.Code != 409 || !jsonContains(rec.Body.String(), "NAMESPACE_NOT_READY") {
		t.Errorf("ns not ready gate = (%d, %s)", rec.Code, rec.Body.String())
	}
	_ = d.DB.Model(&store.Namespace{}).Where("id = ?", nsID).
		Updates(map[string]any{"ready_for_loadtest": true, "status": "READY"}).Error

	// stop RUNNING → STOPPING (transition needs RUNNING first: PROVISIONING → STOPPING allowed too)
	rec = doLT(t, d, withKey(h, "k4"), http.MethodPost, nsID, "/"+lt.ID+"/stop", nil)
	if rec.Code != 202 {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body.String())
	}
	if len(fr.stopped) != 1 {
		t.Errorf("runner stop calls = %v", fr.stopped)
	}
	// double stop → 409 (no longer active)
	rec = doLT(t, d, withKey(h, "k5"), http.MethodPost, nsID, "/"+lt.ID+"/stop", nil)
	if rec.Code != 409 {
		t.Errorf("double stop = %d, want 409", rec.Code)
	}

	// events recorded
	rec = doLT(t, d, h, http.MethodGet, nsID, "/"+lt.ID+"/events", nil)
	if rec.Code != 200 || !jsonContains(rec.Body.String(), "STOPPING") {
		t.Errorf("events = (%d, %s)", rec.Code, rec.Body.String())
	}
}

func jsonContains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func withKey(h map[string]string, key string) map[string]string {
	out := map[string]string{"Idempotency-Key": key}
	for k, v := range h {
		out[k] = v
	}
	return out
}

func TestReconcilerAdvancesStates(t *testing.T) {
	d, fr := testDeps(t)
	h, nsID := readyFixture(t, d)

	rec := doLT(t, d, withKey(h, "kr1"), http.MethodPost, nsID, "",
		map[string]any{"name": "ltr", "script_id": h["X-Script-Id"]})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var lt store.LoadTest
	_ = json.Unmarshal(rec.Body.Bytes(), &lt)

	rc := Reconciler{DB: d.DB, Runner: fr, Every: time.Hour, Now: time.Now}

	// PROVISIONING + runner RUNNING → RUNNING
	fr.states[*lt.RunnerRef] = "RUNNING"
	rc.tick(context.Background())
	var after store.LoadTest
	_ = d.DB.First(&after, "id = ?", lt.ID)
	if after.Status != "RUNNING" {
		t.Fatalf("after provisioning tick = %s, want RUNNING", after.Status)
	}

	// RUNNING + runner COMPLETED → COMPLETED
	fr.states[*lt.RunnerRef] = "COMPLETED"
	rc.tick(context.Background())
	_ = d.DB.First(&after, "id = ?", lt.ID)
	if after.Status != "COMPLETED" {
		t.Fatalf("after completion tick = %s, want COMPLETED", after.Status)
	}
	if after.FinishedAt == nil {
		t.Error("finished_at must be set")
	}

	// event trail: PROVISIONING? (runner ref update wrote PROVISIONING outside fn) + RUNNING + COMPLETED
	var count int64
	_ = d.DB.Model(&store.LoadTestEvent{}).Where("object_id = ?", lt.ID).Count(&count).Error
	if count < 2 {
		t.Errorf("event rows = %d, want >= 2 (RUNNING, COMPLETED)", count)
	}
}

func TestReconcilerTimeout(t *testing.T) {
	d, _ := testDeps(t)
	h, nsID := readyFixture(t, d)
	rec := doLT(t, d, withKey(h, "kt1"), http.MethodPost, nsID, "",
		map[string]any{"name": "ltt", "script_id": h["X-Script-Id"]})
	var lt store.LoadTest
	_ = json.Unmarshal(rec.Body.Bytes(), &lt)

	past := time.Now().Add(-time.Hour)
	if err := d.DB.Model(&store.LoadTest{}).Where("id = ?", lt.ID).
		Updates(map[string]any{"status": "RUNNING", "timeout_at": &past, "started_at": &past}).Error; err != nil {
		t.Fatal(err)
	}
	rc := Reconciler{DB: d.DB, Runner: &fakeRunner{states: map[string]string{}}, Every: time.Hour, Now: time.Now}
	rc.tick(context.Background())
	var after store.LoadTest
	_ = d.DB.First(&after, "id = ?", lt.ID)
	if after.Status != "TIMEOUT_CLEANED" {
		t.Errorf("status after timeout reap = %s, want TIMEOUT_CLEANED", after.Status)
	}
}
