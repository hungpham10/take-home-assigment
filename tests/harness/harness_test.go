// Package harness_test implements the plan-16 Go application harness:
// integrity/authz/idempotency/fault-tolerance cases driven through the REAL
// server composition (pkgs/testutil.NewServer). Integration tests skip
// without TEST_DATABASE_DSN and hard-fail when CI is set (testutil.TestDB).
package harness_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/loadtest"
	"github.com/hungpham/loadtest-platform/pkgs/runner"
	"github.com/hungpham/loadtest-platform/pkgs/script"
	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

// harnessRunner is a configurable fake Runner: per-ref states and the
// Missing flag (controller-restart simulation) for STATE-MEM-01.
type harnessRunner struct {
	mu      sync.Mutex
	refs    []string
	states  map[string]string
	missing map[string]bool
}

func (f *harnessRunner) Mode() string { return "local" }

func (f *harnessRunner) Provision(_ context.Context, _ runner.Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := "fake-" + strconv.Itoa(len(f.refs))
	f.refs = append(f.refs, ref)
	return ref, nil
}
func (f *harnessRunner) Start(_ context.Context, _ string, _ runner.Spec) error { return nil }
func (f *harnessRunner) Stop(_ context.Context, _ string) error                 { return nil }
func (f *harnessRunner) Status(_ context.Context, ref string) (runner.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return runner.Status{State: f.states[ref], Missing: f.missing[ref]}, nil
}

func newServer(t *testing.T, db *gorm.DB, r runner.Runner, q *script.Queue) http.Handler {
	t.Helper()
	return testutil.NewServer(t, testutil.ServerDeps{
		DB: db, Runner: r, Now: time.Now, ScriptsDir: t.TempDir(), ScriptQ: q,
	})
}

// fullFixture builds namespace READY + VALIDATED script + service with targets
// (rows inserted directly, like the pkgs/* integration tests) and returns the
// owner's principal headers plus the namespace and script IDs.
func fullFixture(t *testing.T, db *gorm.DB) (headers map[string]string, nsID, scriptID string) {
	t.Helper()
	tenantID, userID := testutil.TenantUser(t, db, "user")
	uniq := strconv.FormatInt(time.Now().UnixNano(), 36)
	ns := store.Namespace{
		TenantID: tenantID, OwnerUserID: userID, Name: "ns-" + uniq,
		Status: "READY", ReadyForLoadtest: true, Version: 1, CleanupPolicy: []byte("{}"),
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatal(err)
	}
	sc := store.Script{
		NamespaceID: ns.ID, Name: "s-" + uniq, ContentRef: ns.ID + "/s.js",
		ContentHash: "abc", SizeBytes: 10, Status: "VALIDATED", Version: 1,
	}
	if err := db.Create(&sc).Error; err != nil {
		t.Fatal(err)
	}
	svc := store.Service{
		NamespaceID: ns.ID, Name: "svc-" + uniq, OpenAPISpec: []byte(`{}`), SpecHash: store.StrPtr("h"),
		ParsedSummary: store.JSONBytes(map[string]any{"targets": []map[string]any{
			{"id": "getWidgets", "method": "GET", "path": "/widgets"},
		}}),
		Version: 1,
	}
	if err := db.Create(&svc).Error; err != nil {
		t.Fatal(err)
	}
	return testutil.PrincipalHeaders(tenantID, userID, "user"), ns.ID, sc.ID
}

// createLoadtestViaAPI drives POST /loadtests through the real handler with
// the fake runner (Provision+Start succeed) → 201 PROVISIONING.
func createLoadtestViaAPI(t *testing.T, h http.Handler, headers map[string]string, nsID, scriptID, name string) string {
	t.Helper()
	rec := testutil.Do(t, h, http.MethodPost,
		"/api/loadtest/v1/namespaces/"+nsID+"/loadtests",
		testutil.WithKey(headers, "key-"+name),
		map[string]any{"name": name, "script_id": scriptID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create loadtest: %d: %s", rec.Code, rec.Body.String())
	}
	var lt struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lt); err != nil || lt.ID == "" {
		t.Fatalf("create loadtest body: %s", rec.Body.String())
	}
	return lt.ID
}

// adminOf creates an admin user INSIDE the given tenant and returns its ID.
func adminOf(t *testing.T, db *gorm.DB, tenantID string) string {
	t.Helper()
	uniq := strconv.FormatInt(time.Now().UnixNano(), 36)
	u := store.User{TenantID: tenantID, Email: "adm-" + uniq + "@x.co", DisplayName: "A"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.UserRole{UserID: u.ID, Role: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// waitFor polls cond until true or the timeout elapses (reconciler async ticks).
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// api is a shorthand for a data-plane request (prefixes /api/loadtest/v1).
func api(t *testing.T, h http.Handler, method, path string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return testutil.Do(t, h, method, "/api/loadtest/v1"+path, headers, body)
}

// --- API-AUTHZ-01 -----------------------------------------------------------------

func TestAPIAUTHZ01FailClosedPrincipal(t *testing.T) {
	db := testutil.TestDB(t)
	h := newServer(t, db, &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}, nil)

	// missing X-Tenant-Id → 401
	hdrs := map[string]string{"X-User-Id": "u1", "X-User-Role": "user"}
	if rec := api(t, h, http.MethodGet, "/namespaces", hdrs, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing tenant header: %d, want 401", rec.Code)
	}
	// missing X-User-Id → 401
	hdrs = map[string]string{"X-Tenant-Id": "t1", "X-User-Role": "user"}
	if rec := api(t, h, http.MethodGet, "/namespaces", hdrs, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing user header: %d, want 401", rec.Code)
	}
	// no headers at all → 401 (data plane)
	if rec := api(t, h, http.MethodGet, "/namespaces", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no headers: %d, want 401", rec.Code)
	}
	// admin operator route without principal headers → 401
	rec := testutil.Do(t, h, http.MethodPost, "/api/admin/v1/users/00000000-0000-0000-0000-000000000000/tokens", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("operator route without principal: %d, want 401", rec.Code)
	}
}

// --- API-AUTHZ-02 -----------------------------------------------------------------

func TestAPIAUTHZ02CrossTenantNoLeak(t *testing.T) {
	db := testutil.TestDB(t)
	h := newServer(t, db, &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}, nil)

	headersA, nsID, scriptID := fullFixture(t, db)
	ltID := createLoadtestViaAPI(t, h, headersA, nsID, scriptID, "lt-A")

	tenantB, userB := testutil.TenantUser(t, db, "user")
	headersB := testutil.PrincipalHeaders(tenantB, userB, "user")

	// GET single resources → 404 (no existence leak)
	for _, tc := range []struct{ path, what string }{
		{"/namespaces/" + nsID, "namespace"},
		{"/namespaces/" + nsID + "/scripts/" + scriptID, "script"},
		{"/namespaces/" + nsID + "/loadtests/" + ltID, "loadtest"},
		{"/namespaces/" + nsID + "/events", "namespace events"},
	} {
		if rec := api(t, h, http.MethodGet, tc.path, headersB, nil); rec.Code != http.StatusNotFound {
			t.Errorf("cross-tenant %s: %d, want 404", tc.what, rec.Code)
		}
	}
	// list endpoints must not contain A's IDs
	for _, tc := range []struct{ path, what string }{
		{"/namespaces", "namespace list"},
		{"/namespaces/" + nsID + "/scripts", "script list"},
		{"/namespaces/" + nsID + "/loadtests", "loadtest list"},
	} {
		// B's own request URL carries A's nsID only for the nested lists; the
		// namespace lookup itself 404s, so the body can never contain A's IDs.
		rec := api(t, h, http.MethodGet, tc.path, headersB, nil)
		for _, secret := range []string{nsID, scriptID, ltID} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("%s leaked ID %s: %s", tc.what, secret, rec.Body.String())
			}
		}
	}
	// top-level namespace list of B does not include A's namespace
	rec := api(t, h, http.MethodGet, "/namespaces", headersB, nil)
	if strings.Contains(rec.Body.String(), nsID) {
		t.Errorf("cross-tenant namespace list leaked A's namespace: %s", rec.Body.String())
	}
}

// --- API-AUTHZ-03 -----------------------------------------------------------------

func TestAPIAUTHZ03AdminCrossOwnerMutationAndAudit(t *testing.T) {
	db := testutil.TestDB(t)
	h := newServer(t, db, &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}, nil)

	headersA, nsID, scriptID := fullFixture(t, db)
	var ns store.Namespace
	if err := db.First(&ns, "id = ?", nsID).Error; err != nil {
		t.Fatal(err)
	}
	ltID := createLoadtestViaAPI(t, h, headersA, nsID, scriptID, "lt-admin")

	// admin in the SAME tenant as A can mutate across owners
	adminID := adminOf(t, db, ns.TenantID)
	hA := testutil.PrincipalHeaders(ns.TenantID, adminID, "admin")

	// PATCH namespace → 200
	rec := api(t, h, http.MethodPatch, "/namespaces/"+nsID, withIfMatch(hA, fmt.Sprintf(`"%d"`, ns.Version)),
		map[string]any{"description": "admin edit"})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PATCH namespace: %d: %s", rec.Code, rec.Body.String())
	}
	assertAudit(t, db, "namespace.update", nsID)

	// revalidate script → 202
	rec = api(t, h, http.MethodPost, "/namespaces/"+nsID+"/scripts/"+scriptID+"/validate", hA, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("admin revalidate script: %d: %s", rec.Code, rec.Body.String())
	}
	assertAudit(t, db, "script.revalidate", scriptID)

	// stop loadtest → 202
	rec = api(t, h, http.MethodPost, "/namespaces/"+nsID+"/loadtests/"+ltID+"/stop",
		testutil.WithKey(hA, "stop-admin"), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("admin stop loadtest: %d: %s", rec.Code, rec.Body.String())
	}
	assertAudit(t, db, "loadtest.stop", ltID)

	// admin in a DIFFERENT tenant → 404 on data plane (tenant isolation holds
	// for admins), and 403 on cross-tenant token issue (self-or-admin).
	tenantB, adminB := testutil.TenantUser(t, db, "admin")
	hB := testutil.PrincipalHeaders(tenantB, adminB, "admin")

	for _, tc := range []struct {
		method, path string
		body         any
		what         string
	}{
		{http.MethodPatch, "/namespaces/" + nsID, map[string]any{"description": "x"}, "cross-tenant PATCH"},
		{http.MethodPost, "/namespaces/" + nsID + "/scripts/" + scriptID + "/validate", nil, "cross-tenant revalidate"},
		{http.MethodPost, "/namespaces/" + nsID + "/loadtests/" + ltID + "/stop", nil, "cross-tenant stop"},
	} {
		rec := api(t, h, tc.method, tc.path, testutil.WithKey(hB, "k-"+tc.what), tc.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404 (%s)", tc.what, rec.Code, rec.Body.String())
		}
	}
	// cross-tenant admin issuing a token for A's user → 403 (fail-closed;
	// canary for the RequireSelfOrAdmin invariant in adminapi.issueToken)
	rec = testutil.Do(t, h, http.MethodPost, "/api/admin/v1/users/"+headersA["X-User-Id"]+"/tokens", hB, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-tenant token issue: %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

// API-AUTHZ-03 canary companion: a NON-admin user in the same tenant cannot
// issue tokens for another user. This is the assertion that flips the suite
// red when the RequireSelfOrAdmin check is removed from issueToken.
func TestAPIAUTHZ03SelfOnlyTokenGate(t *testing.T) {
	db := testutil.TestDB(t)
	h := newServer(t, db, &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}, nil)

	headersA, _, _ := fullFixture(t, db)
	tenantID := headersA["X-Tenant-Id"]

	other := store.User{TenantID: tenantID, Email: "o-" + tenantID + "@x.co", DisplayName: "O"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.UserRole{UserID: other.ID, Role: "user"}).Error; err != nil {
		t.Fatal(err)
	}
	hO := testutil.PrincipalHeaders(tenantID, other.ID, "user")
	rec := testutil.Do(t, h, http.MethodPost, "/api/admin/v1/users/"+headersA["X-User-Id"]+"/tokens", hO, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-only token gate: %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

func withIfMatch(h map[string]string, etag string) map[string]string {
	out := make(map[string]string, len(h)+1)
	for k, v := range h {
		out[k] = v
	}
	out["If-Match"] = etag
	return out
}

func assertAudit(t *testing.T, db *gorm.DB, action, objectID string) {
	t.Helper()
	var count int64
	if err := db.Model(&store.AuditLog{}).
		Where("action = ? AND object_id = ?", action, objectID).
		Count(&count).Error; err != nil || count == 0 {
		t.Fatalf("audit row missing for %s/%s (count=%d err=%v)", action, objectID, count, err)
	}
}

// --- API-AUTHZ-04 -----------------------------------------------------------------

// Spoofed X-User-Role=admin with DB role=user must fail closed: the middleware
// resolves the role server-side and the admin-only mutation is rejected.
func TestAPIAUTHZ04SpoofedRoleHeader(t *testing.T) {
	db := testutil.TestDB(t)
	h := newServer(t, db, &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}, nil)

	headersA, _, _ := fullFixture(t, db)
	tenantID := headersA["X-Tenant-Id"]

	victim := store.User{TenantID: tenantID, Email: "v-" + tenantID + "@x.co", DisplayName: "V"}
	if err := db.Create(&victim).Error; err != nil {
		t.Fatal(err)
	}

	// A (DB role=user) spoofs the admin header and tries to issue a token for
	// another user — admin-only mutation → 403, not honored from the header.
	spoof := map[string]string{
		"X-Tenant-Id": tenantID, "X-User-Id": headersA["X-User-Id"], "X-User-Role": "admin",
	}
	rec := testutil.Do(t, h, http.MethodPost, "/api/admin/v1/users/"+victim.ID+"/tokens", spoof, nil)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed admin role: %d, want 403/401 (%s)", rec.Code, rec.Body.String())
	}
}

// --- API-IDEM-01 -------------------------------------------------------------------

func TestAPIIDEM01ReplayAndConflict(t *testing.T) {
	db := testutil.TestDB(t)
	h := newServer(t, db, &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}, nil)

	headers, _, _ := fullFixture(t, db)
	name := "idem-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	payload := map[string]any{"name": name}

	rec := api(t, h, http.MethodPost, "/namespaces", testutil.WithKey(headers, "k-idem"), payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d: %s", rec.Code, rec.Body.String())
	}
	var ns1 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ns1)

	// same key + same payload → replay with the SAME object id
	rec = api(t, h, http.MethodPost, "/namespaces", testutil.WithKey(headers, "k-idem"), payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("replay status: %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Error("Idempotency-Replayed header missing on replay")
	}
	var ns2 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ns2)
	if ns1.ID == "" || ns1.ID != ns2.ID {
		t.Errorf("replay id mismatch: %s vs %s", ns1.ID, ns2.ID)
	}

	// same key + different payload → 422
	rec = api(t, h, http.MethodPost, "/namespaces", testutil.WithKey(headers, "k-idem"),
		map[string]any{"name": name + "-different"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("key reuse with different payload: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
}

// --- API-CONC-01 -------------------------------------------------------------------

// 10 concurrent PATCHes with the same If-Match: exactly one 200, the rest
// 412/409 (optimistic concurrency must never double-apply).
func TestAPICONC01OptimisticConcurrency(t *testing.T) {
	db := testutil.TestDB(t)
	h := newServer(t, db, &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}, nil)

	headers, nsID, _ := fullFixture(t, db)
	var ns store.Namespace
	if err := db.First(&ns, "id = ?", nsID).Error; err != nil {
		t.Fatal(err)
	}

	const n = 10
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hdrs := withIfMatch(headers, fmt.Sprintf(`"%d"`, ns.Version))
			rec := testutil.Do(t, h, http.MethodPatch, "/api/loadtest/v1/namespaces/"+nsID, hdrs,
				map[string]any{"description": fmt.Sprintf("conc-%d", i)})
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	ok, failed := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusPreconditionFailed, http.StatusConflict:
			failed++
		default:
			t.Fatalf("unexpected status %d (codes=%v)", c, codes)
		}
	}
	if ok != 1 || failed != n-1 {
		t.Errorf("got %d x200 and %d x412/409, want exactly 1 success and %d failures (codes=%v)",
			ok, failed, n-1, codes)
	}
	var after store.Namespace
	if err := db.First(&after, "id = ?", nsID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Version != ns.Version+1 {
		t.Errorf("namespace version = %d, want %d (exactly one apply)", after.Version, ns.Version+1)
	}
}

// --- API-VAL-01 --------------------------------------------------------------------

// Broken service spec → namespace validate returns 422 with blocking_reasons and
// marks the namespace INVALID (fail-closed readiness gates).
func TestAPIVAL01BrokenSpecInvalid(t *testing.T) {
	db := testutil.TestDB(t)
	h := newServer(t, db, &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}, nil)

	headers, _, _ := fullFixture(t, db)
	// force a fresh DRAFT namespace (fixture ns is READY)
	draft := store.Namespace{
		TenantID: headers["X-Tenant-Id"], OwnerUserID: headers["X-User-Id"],
		Name:   "draft-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Status: "DRAFT", Version: 1, CleanupPolicy: []byte("{}"),
	}
	if err := db.Create(&draft).Error; err != nil {
		t.Fatal(err)
	}
	// openapi_spec is a JSONB column — a "broken" spec is still valid JSON but
	// fails OpenAPI semantic validation (undeclared path parameter).
	broken := store.Service{
		NamespaceID: draft.ID, Name: "broken",
		OpenAPISpec: []byte(`{"openapi":"3.0.0","info":{"title":"x","version":"1"},
	         "paths":{"/items/{id}":{"get":{"responses":{"200":{"description":"ok"}}}}}}`),
		ParsedSummary: store.JSONBytes(map[string]any{}), Version: 1,
	}
	if err := db.Create(&broken).Error; err != nil {
		t.Fatal(err)
	}

	rec := api(t, h, http.MethodPost, "/namespaces/"+draft.ID+"/validate", headers, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validate with broken spec: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		BlockingReasons []string `json:"blocking_reasons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.BlockingReasons) == 0 {
		t.Errorf("response must carry blocking_reasons: %s", rec.Body.String())
	}
	var after store.Namespace
	if err := db.First(&after, "id = ?", draft.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != "INVALID" {
		t.Errorf("namespace status = %s, want INVALID", after.Status)
	}
}

// --- STATE-QUEUE-01 ----------------------------------------------------------------

// Full validation queue → Submit false → 429 and the script status is left
// untouched (never stuck in VALIDATING; re-triggerable).
func TestSTATEQUEUE01FullQueueRejects429(t *testing.T) {
	db := testutil.TestDB(t)
	// workers=0, buffer=0 → the queue is permanently full.
	q := script.NewQueue(context.Background(), db, t.TempDir(), 0, 0)
	h := newServer(t, db, &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}, q)

	headers, nsID, _ := fullFixture(t, db)
	sc := store.Script{
		NamespaceID: nsID, Name: "q-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		ContentRef: nsID + "/q.js", ContentHash: "abc", SizeBytes: 10,
		Status: "PENDING_VALIDATION", Version: 1,
	}
	if err := db.Create(&sc).Error; err != nil {
		t.Fatal(err)
	}

	rec := api(t, h, http.MethodPost, "/namespaces/"+nsID+"/scripts/"+sc.ID+"/validate", headers, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("full queue validate: %d, want 429 (%s)", rec.Code, rec.Body.String())
	}
	var after store.Script
	if err := db.First(&after, "id = ?", sc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != "PENDING_VALIDATION" || after.Version != 1 {
		t.Errorf("script mutated on full queue: status=%s version=%d", after.Status, after.Version)
	}
	var events int64
	db.Table("tbl_script_events").Where("object_id = ?", sc.ID).Count(&events)
	if events != 0 {
		t.Errorf("validation events appended despite full queue: %d", events)
	}
}

// --- STATE-MEM-01 ------------------------------------------------------------------

// Runner lost the ref (controller restart wiped its in-process table) →
// reconciler must fail the run FAILED with a "runner lost" reason instead of
// polling a dead ref forever.
func TestSTATEMEM01RunnerLostFailsClosed(t *testing.T) {
	db := testutil.TestDB(t)
	fr := &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}
	h := newServer(t, db, fr, nil)

	headers, nsID, scriptID := fullFixture(t, db)
	ltID := createLoadtestViaAPI(t, h, headers, nsID, scriptID, "lt-mem")

	var lt store.LoadTest
	if err := db.First(&lt, "id = ?", ltID).Error; err != nil {
		t.Fatal(err)
	}
	if lt.Status != "PROVISIONING" || lt.RunnerRef == nil {
		t.Fatalf("expected PROVISIONING with runner_ref, got %s/%v", lt.Status, lt.RunnerRef)
	}

	// Simulate the restart: the runner no longer knows the ref.
	fr.mu.Lock()
	fr.missing[*lt.RunnerRef] = true
	fr.mu.Unlock()

	rc := loadtest.Reconciler{DB: db, Runner: fr, Every: 5 * time.Millisecond, Now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rc.Start(ctx)

	waitFor(t, 3*time.Second, func() bool {
		var cur store.LoadTest
		if err := db.First(&cur, "id = ?", ltID).Error; err != nil {
			return false
		}
		return cur.Status == "FAILED"
	}, "loadtest not FAILED after runner-lost ticks")

	var ev store.LoadTestEvent
	if err := db.Where("object_id = ? AND to_status = ?", ltID, "FAILED").
		Order("id DESC").First(&ev).Error; err != nil {
		t.Fatalf("FAILED event missing: %v", err)
	}
	if ev.Reason == nil || !strings.Contains(*ev.Reason, "runner lost") {
		t.Errorf("event reason = %v, want it to mention runner lost", ev.Reason)
	}
}

// --- RUN-STOP ----------------------------------------------------------------------

// LocalRunner.Stop on an unknown ref is idempotent (nil, no panic).
func TestRUNSTOP01StopUnknownRefIdempotent(t *testing.T) {
	lr := runner.NewLocal("")
	if err := lr.Stop(context.Background(), "no-such-ref"); err != nil {
		t.Errorf("Stop(unknown ref) = %v, want nil", err)
	}
	// double stop on the same unknown ref stays nil
	if err := lr.Stop(context.Background(), "no-such-ref"); err != nil {
		t.Errorf("second Stop(unknown ref) = %v, want nil", err)
	}
}

// --- RUN-TIMEOUT -------------------------------------------------------------------

// reapTimeouts: RUNNING past timeout_at → TIMED_OUT → TIMEOUT_CLEANED.
func TestRUNTIMEOUT01ReapTimeouts(t *testing.T) {
	db := testutil.TestDB(t)
	fr := &harnessRunner{states: map[string]string{}, missing: map[string]bool{}}
	h := newServer(t, db, fr, nil)

	headers, nsID, scriptID := fullFixture(t, db)
	ltID := createLoadtestViaAPI(t, h, headers, nsID, scriptID, "lt-timeout")

	// Force RUNNING with a past timeout_at (reaper condition).
	past := time.Now().Add(-time.Minute)
	if err := db.Model(&store.LoadTest{}).Where("id = ?", ltID).
		Updates(map[string]any{"status": "RUNNING", "timeout_at": &past}).Error; err != nil {
		t.Fatal(err)
	}

	rc := loadtest.Reconciler{DB: db, Runner: fr, Every: 5 * time.Millisecond, Now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rc.Start(ctx)

	waitFor(t, 3*time.Second, func() bool {
		var cur store.LoadTest
		if err := db.First(&cur, "id = ?", ltID).Error; err != nil {
			return false
		}
		return cur.Status == "TIMEOUT_CLEANED"
	}, "loadtest not TIMEOUT_CLEANED after reap ticks")
}
