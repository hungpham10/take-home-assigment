package loadtest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/loadtest"
	"github.com/hungpham/loadtest-platform/pkgs/runner"
	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
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

// Handler tests go through the REAL server composition (server.NewRouter via
// testutil) — /api/loadtest/v1/namespaces/{id}/loadtests exactly as nginx
// routes them. Skips locally without Postgres, fails in CI.

func testServer(t *testing.T) (h http.Handler, db *gorm.DB, fr *fakeRunner) {
	t.Helper()
	dbr := testutil.TestDB(t)
	fr = &fakeRunner{}
	h = testutil.NewServer(t, testutil.ServerDeps{DB: dbr, Runner: fr, Now: time.Now})
	return h, dbr, fr
}

// doLT performs a request against the loadtest routes of nsID.
func doLT(t *testing.T, h http.Handler, headers map[string]string, method, nsID, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return testutil.Do(t, h, method, "/api/loadtest/v1/namespaces/"+nsID+"/loadtests"+path, headers, body)
}

// readyFixture: tenant+user+READY namespace+VALIDATED script+service with targets.
func readyFixture(t *testing.T, db *gorm.DB) (map[string]string, string) {
	t.Helper()
	tenantID, userID := testutil.TenantUser(t, db, "user")
	ns := store.Namespace{
		TenantID: tenantID, OwnerUserID: userID, Name: "ns" + itoa(time.Now().UnixNano()),
		Status: "READY", ReadyForLoadtest: true, Version: 1, CleanupPolicy: []byte("{}"),
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatal(err)
	}
	sc := store.Script{
		NamespaceID: ns.ID, Name: "s" + itoa(time.Now().UnixNano()), ContentRef: ns.ID + "/s.js",
		ContentHash: "abc", SizeBytes: 10, Status: "VALIDATED", Version: 1,
	}
	if err := db.Create(&sc).Error; err != nil {
		t.Fatal(err)
	}
	svc := store.Service{
		NamespaceID: ns.ID, Name: "svc", OpenAPISpec: []byte(`{}`), SpecHash: store.StrPtr("h"),
		ParsedSummary: store.JSONBytes(map[string]any{"targets": []map[string]any{
			{"id": "getWidgets", "method": "GET", "path": "/widgets"},
		}}), Version: 1,
	}
	if err := db.Create(&svc).Error; err != nil {
		t.Fatal(err)
	}
	h := testutil.PrincipalHeaders(tenantID, userID, "user")
	h["X-Script-Id"] = sc.ID // test-only channel
	return h, ns.ID
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

func TestINT_LT_01_LoadTestGatesAndLifecycle(t *testing.T) {
	h, db, fr := testServer(t)
	headers, nsID := readyFixture(t, db)

	// missing Idempotency-Key → 422
	rec := doLT(t, h, headers, http.MethodPost, nsID, "", map[string]any{"name": "lt1", "script_id": headers["X-Script-Id"]})
	if rec.Code != 422 {
		t.Fatalf("missing key: %d", rec.Code)
	}

	// create → PROVISIONING
	rec = doLT(t, h, testutil.WithKey(headers, "k1"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt1", "script_id": headers["X-Script-Id"]})
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
	_ = db.First(&badScript, "id = ?", headers["X-Script-Id"]).Error
	_ = db.Model(&store.Script{}).Where("id = ?", badScript.ID).Update("status", "INVALID").Error
	rec = doLT(t, h, testutil.WithKey(headers, "k2"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt2", "script_id": badScript.ID})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "SCRIPT_NOT_VALIDATED") {
		t.Errorf("invalid script gate = (%d, %s)", rec.Code, rec.Body.String())
	}
	_ = db.Model(&store.Script{}).Where("id = ?", badScript.ID).Update("status", "VALIDATED").Error

	// gate: namespace not READY → 409
	_ = db.Model(&store.Namespace{}).Where("id = ?", nsID).
		Updates(map[string]any{"ready_for_loadtest": false, "status": "DEGRADED"}).Error
	rec = doLT(t, h, testutil.WithKey(headers, "k3"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt3", "script_id": headers["X-Script-Id"]})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "NAMESPACE_NOT_READY") {
		t.Errorf("ns not ready gate = (%d, %s)", rec.Code, rec.Body.String())
	}
	_ = db.Model(&store.Namespace{}).Where("id = ?", nsID).
		Updates(map[string]any{"ready_for_loadtest": true, "status": "READY"}).Error

	// stop: PROVISIONING → STOPPING (transition map allows it)
	rec = doLT(t, h, testutil.WithKey(headers, "k4"), http.MethodPost, nsID, "/"+lt.ID+"/stop", nil)
	if rec.Code != 202 {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body.String())
	}
	if len(fr.stopped) != 1 {
		t.Errorf("runner stop calls = %v", fr.stopped)
	}
	// double stop → 409 (no longer active)
	rec = doLT(t, h, testutil.WithKey(headers, "k5"), http.MethodPost, nsID, "/"+lt.ID+"/stop", nil)
	if rec.Code != 409 {
		t.Errorf("double stop = %d, want 409", rec.Code)
	}

	// events recorded
	rec = doLT(t, h, headers, http.MethodGet, nsID, "/"+lt.ID+"/events", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "STOPPING") {
		t.Errorf("events = (%d, %s)", rec.Code, rec.Body.String())
	}
}

// TestINT_LT_03_LoadTestTargetsCarryServiceName — regression: run-scoped
// NetworkPolicy selects target pods by loadtest.io/component-id=<service name>,
// so collectTargets must stamp service_name onto every target (service_id is
// the server-generated "svc-<rand>" id and is NOT a pod label).
func TestINT_LT_03_LoadTestTargetsCarryServiceName(t *testing.T) {
	h, db, _ := testServer(t)
	headers, nsID := readyFixture(t, db) // service Name = "svc"

	rec := doLT(t, h, testutil.WithKey(headers, "k-svcname"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt-svcname", "script_id": headers["X-Script-Id"]})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var lt store.LoadTest
	if err := json.Unmarshal(rec.Body.Bytes(), &lt); err != nil {
		t.Fatalf("decode loadtest: %v", err)
	}
	var targets []map[string]any
	if err := json.Unmarshal(lt.Targets, &targets); err != nil || len(targets) == 0 {
		t.Fatalf("targets = %s", lt.Targets)
	}
	for _, tgt := range targets {
		name, ok := tgt["service_name"].(string)
		if !ok || name != "svc" {
			t.Fatalf("target %v missing service_name=svc (got %v)", tgt, name)
		}
	}
}

// testServerK8S — server composition with the cluster wired (plans/21 gates).
func testServerK8S(t *testing.T, staleAfter time.Duration) (http.Handler, *gorm.DB) {
	t.Helper()
	dbr := testutil.TestDB(t)
	h := testutil.NewServer(t, testutil.ServerDeps{DB: dbr, Runner: &fakeRunner{}, Now: time.Now, K8S: true, StaleAfter: staleAfter})
	return h, dbr
}

// TestCreateStatusStaleGate — plans/21 WATCH-05: with the cluster wired, a
// READY namespace whose watcher heartbeat is missing/old returns 409
// NAMESPACE_NOT_READY reason STATUS_STALE (fail-closed).
func TestINT_LT_02_CreateStatusStaleGate(t *testing.T) {
	h, db := testServerK8S(t, 15*time.Minute)
	headers, nsID := readyFixture(t, db)

	rec := doLT(t, h, testutil.WithKey(headers, "k-stale"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt-stale", "script_id": headers["X-Script-Id"]})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "STATUS_STALE") {
		t.Fatalf("stale heartbeat: %d %s", rec.Code, rec.Body.String())
	}

	// Fresh heartbeat passes the gate.
	if err := db.Model(&store.Namespace{}).Where("id = ?", nsID).
		Update("status_observed_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	rec = doLT(t, h, testutil.WithKey(headers, "k-fresh"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt-fresh", "script_id": headers["X-Script-Id"]})
	if rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "STATUS_STALE") {
		t.Fatalf("fresh heartbeat blocked: %d %s", rec.Code, rec.Body.String())
	}
}

// TestGetUnreliableFlag — plans/22 DEGRADED-with-running-run (docs §6): a
// namespace DEGRADED event after the run's started_at flags the run
// "unreliable": true in GET /loadtests/{id} and /events. The run keeps
// running (the watcher never stops it).
func TestINT_LT_03_GetUnreliableFlag(t *testing.T) {
	h, db, _ := testServer(t)
	headers, nsID := readyFixture(t, db)

	rec := doLT(t, h, testutil.WithKey(headers, "k1"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt-unreliable", "script_id": headers["X-Script-Id"]})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var lt store.LoadTest
	_ = json.Unmarshal(rec.Body.Bytes(), &lt)

	// No degradation yet → not unreliable.
	rec = doLT(t, h, headers, http.MethodGet, nsID, "/"+lt.ID, nil)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["unreliable"] == true {
		t.Fatal("unreliable before any DEGRADED event")
	}

	// Namespace degrades AFTER the run started.
	if err := db.Exec(
		`INSERT INTO tbl_namespace_events (object_id, to_status, actor, metadata)
		 VALUES (?, 'DEGRADED', '{}', jsonb_build_object('namespace_degraded_at', now()))`,
		nsID).Error; err != nil {
		t.Fatal(err)
	}
	rec = doLT(t, h, headers, http.MethodGet, nsID, "/"+lt.ID, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["unreliable"] != true {
		t.Fatalf("unreliable flag missing after DEGRADED: %v", got)
	}

	rec = doLT(t, h, headers, http.MethodGet, nsID, "/"+lt.ID+"/events", nil)
	if !strings.Contains(rec.Body.String(), `"unreliable":true`) {
		t.Fatalf("events payload missing unreliable flag: %s", rec.Body.String())
	}
}

// TestINT_LT_04_LoadConfigCapsRejectBeforeWrite — plans/42: an out-of-range
// load_config is rejected with 422 at the edge, BEFORE any row is written and
// before the Runner is touched. At-limit configs still create normally.
func TestINT_LT_04_LoadConfigCapsRejectBeforeWrite(t *testing.T) {
	db := testutil.TestDB(t)
	fr := &fakeRunner{}
	h := testutil.NewServer(t, testutil.ServerDeps{
		DB: db, Runner: fr, Now: time.Now,
		Limits: loadtest.Limits{MaxVUs: 10, MaxDuration: 2 * time.Minute, MaxTimeoutMinutes: 10},
	})
	headers, nsID := readyFixture(t, db)
	scriptID := headers["X-Script-Id"]

	countRows := func() int64 {
		var n int64
		if err := db.Model(&store.LoadTest{}).Where("namespace_id = ?", nsID).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countRows()

	cases := []struct {
		name     string
		cfg      map[string]any
		wantCode string
	}{
		{"vus over cap", map[string]any{"vusers": 11}, "VUS_LIMIT_EXCEEDED"},
		{"duration over cap", map[string]any{"vusers": 5, "duration": "3m", "timeout_minutes": 5}, "DURATION_LIMIT_EXCEEDED"},
		{"unparseable duration", map[string]any{"vusers": 5, "duration": "banana"}, "INVALID_DURATION"},
		{"timeout below duration", map[string]any{"vusers": 5, "duration": "90s", "timeout_minutes": 1}, "TIMEOUT_TOO_SHORT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doLT(t, h, testutil.WithKey(headers, "cap-"+tc.name), http.MethodPost, nsID, "",
				map[string]any{"name": "lt-" + tc.name, "script_id": scriptID, "load_config": tc.cfg})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d (%s), want 422", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Errorf("body = %s, want code %s", rec.Body.String(), tc.wantCode)
			}
			if got := countRows(); got != before {
				t.Errorf("row written for rejected config: %d -> %d", before, got)
			}
			if len(fr.provisioned) != 0 {
				t.Errorf("runner touched for rejected config: %v", fr.provisioned)
			}
		})
	}

	// At-limit values are legal.
	rec := doLT(t, h, testutil.WithKey(headers, "cap-ok"), http.MethodPost, nsID, "",
		map[string]any{"name": "lt-at-limit", "script_id": scriptID,
			"load_config": map[string]any{"vusers": 10, "duration": "2m", "timeout_minutes": 10}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("at-limit create = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	if got := countRows(); got != before+1 {
		t.Errorf("rows after valid create = %d, want %d", got, before+1)
	}
}
