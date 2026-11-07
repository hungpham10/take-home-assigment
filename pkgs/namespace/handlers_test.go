package namespace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	platform "github.com/hungpham/loadtest-platform"
	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Integration tests (opsense-rs style): skip when Postgres unreachable
// locally, hard-fail in CI (TEST_DATABASE_DSN + CI set).

func testDeps(t *testing.T) Deps {
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
	return Deps{DB: db, Now: func() time.Time { return time.Now() }}
}

// userFixture creates a tenant + user (+role) and returns principal headers.
func userFixture(t *testing.T, d Deps, role string) map[string]string {
	t.Helper()
	var tenantID, userID string
	err := d.DB.Raw(`
		WITH t AS (
			INSERT INTO tbl_tenants (name, slug) VALUES ('Ns', 'ns-' || gen_random_uuid()::text)
			RETURNING id
		), u AS (
			INSERT INTO tbl_users (tenant_id, email, display_name)
			SELECT t.id, 'ns-' || gen_random_uuid()::text || '@x.co', 'Ns' FROM t
			RETURNING id, tenant_id
		), r AS (
			INSERT INTO tbl_user_roles (user_id, role) SELECT id, ? FROM u
		)
		SELECT u.tenant_id::text, u.id::text FROM u`, role).
		Row().Scan(&tenantID, &userID)
	if err != nil {
		t.Fatalf("userFixture: %v", err)
	}
	return map[string]string{"X-Tenant-Id": tenantID, "X-User-Id": userID, "X-User-Role": role}
}

func do(t *testing.T, d Deps, method, path string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(marshalJSON(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	authz.RequirePrincipal(Router(d)).ServeHTTP(rec, req)
	return rec
}

func TestNamespaceCRUDAndETag(t *testing.T) {
	d := testDeps(t)
	h := userFixture(t, d, "user")

	// missing idempotency key → 422
	rec := do(t, d, http.MethodPost, "/", h, map[string]string{"name": "ns1"})
	if rec.Code != 422 {
		t.Fatalf("missing Idempotency-Key: status = %d", rec.Code)
	}

	// create
	rec = do(t, d, http.MethodPost, "/", withKey(h, "k1"), map[string]string{"name": "ns1", "description": "first"})
	if rec.Code != 201 {
		t.Fatalf("create: status = %d: %s", rec.Code, rec.Body.String())
	}
	var ns namespaceRow
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)
	if ns.Status != "DRAFT" || ns.Version != 1 {
		t.Errorf("created = %+v", ns)
	}
	etag := rec.Header().Get("ETag")
	if etag != `"1"` {
		t.Errorf("ETag = %q", etag)
	}

	// duplicate name → 422
	rec = do(t, d, http.MethodPost, "/", withKey(h, "k2"), map[string]string{"name": "ns1"})
	if rec.Code != 422 {
		t.Errorf("duplicate create: status = %d, want 422", rec.Code)
	}

	// get with ETag
	rec = do(t, d, http.MethodGet, "/"+ns.ID, h, nil)
	if rec.Code != 200 || rec.Header().Get("ETag") != etag {
		t.Errorf("get = (%d, %q)", rec.Code, rec.Header().Get("ETag"))
	}

	// stale If-Match → 412
	rec = do(t, d, http.MethodPatch, "/"+ns.ID, staleIfMatch(h, `"99"`),
		map[string]any{"description": "x"})
	if rec.Code != 412 {
		t.Errorf("stale If-Match: status = %d, want 412", rec.Code)
	}

	// patch with correct If-Match
	rec = do(t, d, http.MethodPatch, "/"+ns.ID, staleIfMatch(h, etag),
		map[string]any{"description": "updated"})
	if rec.Code != 200 {
		t.Fatalf("patch: status = %d: %s", rec.Code, rec.Body.String())
	}
	var patched namespaceRow
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched.Description != "updated" || patched.Version != 2 {
		t.Errorf("patched = %+v", patched)
	}

	// idempotent replay returns identical response
	rec1 := do(t, d, http.MethodPost, "/", withKey(h, "k3"), map[string]string{"name": "ns2"})
	rec2 := do(t, d, http.MethodPost, "/", withKey(h, "k3"), map[string]string{"name": "ns2"})
	if rec1.Code != 201 || rec2.Code != 201 || rec2.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("replay: rec1=%d rec2=%d replayed=%q",
			rec1.Code, rec2.Code, rec2.Header().Get("Idempotency-Replayed"))
	}
	var ns2a, ns2b namespaceRow
	_ = json.Unmarshal(rec1.Body.Bytes(), &ns2a)
	_ = json.Unmarshal(rec2.Body.Bytes(), &ns2b)
	if ns2a.ID != ns2b.ID {
		t.Errorf("replay returned different object: %s vs %s", ns2a.ID, ns2b.ID)
	}

	// same key different payload → 422
	rec = do(t, d, http.MethodPost, "/", withKey(h, "k3"), map[string]string{"name": "ns9"})
	if rec.Code != 422 {
		t.Errorf("key reuse with different payload: status = %d, want 422", rec.Code)
	}

	// delete (DRAFT → DELETING) → 202
	rec = do(t, d, http.MethodDelete, "/"+ns.ID, h, nil)
	if rec.Code != 202 {
		t.Fatalf("delete: status = %d: %s", rec.Code, rec.Body.String())
	}

	// after delete: status DELETING (reaper finishes to DELETED), still visible
	rec = do(t, d, http.MethodGet, "/"+ns.ID, h, nil)
	if rec.Code != 200 {
		t.Errorf("get after delete: status = %d, want 200", rec.Code)
	}
	var deleted namespaceRow
	_ = json.Unmarshal(rec.Body.Bytes(), &deleted)
	if deleted.Status != "DELETING" {
		t.Errorf("status after delete = %q, want DELETING", deleted.Status)
	}

	// events endpoint still serves the namespace
	rec = do(t, d, http.MethodGet, "/"+ns.ID+"/events", h, nil)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte("DELETING")) {
		t.Errorf("events after delete = (%d, %s)", rec.Code, rec.Body.String())
	}
	// delete transition recorded in events table (DRAFT -> DELETING)
	var eventCount int
	if err := d.DB.Raw(`SELECT count(*) FROM tbl_namespace_events WHERE object_id = ?::uuid AND to_status = 'DELETING'`,
		ns.ID).Row().Scan(&eventCount); err != nil || eventCount != 1 {
		t.Errorf("delete event count = %d, err = %v", eventCount, err)
	}
}

func TestOwnerIsolationAndAdminOverride(t *testing.T) {
	d := testDeps(t)
	owner := userFixture(t, d, "user")
	admin := userFixture(t, d, "admin")
	other := userFixture(t, d, "user")

	rec := do(t, d, http.MethodPost, "/", withKey(owner, "ko1"), map[string]string{"name": "owned"})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var ns namespaceRow
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)

	// other user cannot see it (404 — no existence leak)
	if rec = do(t, d, http.MethodGet, "/"+ns.ID, other, nil); rec.Code != 404 {
		t.Errorf("cross-owner get: status = %d, want 404", rec.Code)
	}
	// other user does not see it in list either
	rec = do(t, d, http.MethodGet, "/", other, nil)
	if bytes.Contains(rec.Body.Bytes(), []byte(ns.ID)) {
		t.Error("cross-owner list leaked namespace")
	}
	// admin in a DIFFERENT tenant also cannot (tenant isolation always)
	if rec = do(t, d, http.MethodGet, "/"+ns.ID, admin, nil); rec.Code != 404 {
		t.Errorf("cross-tenant admin get: status = %d, want 404", rec.Code)
	}

	// admin in the SAME tenant can manage across owners
	adminHeaders := sameTenantAdmin(t, d, owner["X-Tenant-Id"])
	rec = do(t, d, http.MethodGet, "/"+ns.ID, adminHeaders, nil)
	if rec.Code != 200 {
		t.Errorf("same-tenant admin get: status = %d, want 200", rec.Code)
	}
}

func withKey(h map[string]string, key string) map[string]string {
	out := map[string]string{"Idempotency-Key": key}
	for k, v := range h {
		out[k] = v
	}
	return out
}

func staleIfMatch(h map[string]string, etag string) map[string]string {
	out := map[string]string{"If-Match": etag}
	for k, v := range h {
		out[k] = v
	}
	return out
}

func sameTenantAdmin(t *testing.T, d Deps, tenantID string) map[string]string {
	t.Helper()
	var userID string
	if err := d.DB.Raw(`
		WITH u AS (
			INSERT INTO tbl_users (tenant_id, email, display_name)
			VALUES (?::uuid, 'adm-' || gen_random_uuid()::text || '@x.co', 'A')
			RETURNING id
		), r AS (INSERT INTO tbl_user_roles (user_id, role) SELECT id, 'admin' FROM u)
		SELECT id::text FROM u`, tenantID).Row().Scan(&userID); err != nil {
		return nil
	}
	return map[string]string{
		"X-Tenant-Id": tenantID, "X-User-Id": userID, "X-User-Role": "admin",
	}
}
