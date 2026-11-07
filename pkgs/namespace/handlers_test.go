package namespace_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

// Integration tests (opsense-rs style): skip when Postgres unreachable
// locally, hard-fail in CI (TEST_DATABASE_DSN + CI set). All requests go
// through the REAL server composition (server.NewRouter) — tests exercise
// /api/loadtest/v1/namespaces exactly as nginx routes them.

func testServer(t *testing.T) (http.Handler, *gorm.DB) {
	t.Helper()
	db := testutil.TestDB(t)
	h := testutil.NewServer(t, testutil.ServerDeps{DB: db, Now: time.Now})
	return h, db
}

// staleIfMatch returns headers plus an If-Match header.
func staleIfMatch(h map[string]string, etag string) map[string]string {
	out := map[string]string{"If-Match": etag}
	for k, v := range h {
		out[k] = v
	}
	return out
}

// sameTenantAdmin creates an admin user in an existing tenant; returns headers.
func sameTenantAdmin(t *testing.T, db *gorm.DB, tenantID string) map[string]string {
	t.Helper()
	suffix := time.Now().UnixNano()
	user := store.User{TenantID: tenantID, Email: "adm-" + itoa(suffix) + "@x.co", DisplayName: "A"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.UserRole{UserID: user.ID, Role: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	return testutil.PrincipalHeaders(tenantID, user.ID, "admin")
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

func TestNamespaceCRUDAndETag(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	// missing idempotency key → 422
	rec := testutil.Do(t, h, http.MethodPost, base, principal, map[string]string{"name": "ns1"})
	if rec.Code != 422 {
		t.Fatalf("missing Idempotency-Key: status = %d", rec.Code)
	}

	// create
	rec = testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "k1"),
		map[string]string{"name": "ns1", "description": "first"})
	if rec.Code != 201 {
		t.Fatalf("create: status = %d: %s", rec.Code, rec.Body.String())
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)
	if ns.Status != "DRAFT" || ns.Version != 1 {
		t.Errorf("created = %+v", ns)
	}
	etag := rec.Header().Get("ETag")
	if etag != `"1"` {
		t.Errorf("ETag = %q", etag)
	}

	// duplicate name → 422
	rec = testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "k2"), map[string]string{"name": "ns1"})
	if rec.Code != 422 {
		t.Errorf("duplicate create: status = %d, want 422", rec.Code)
	}

	// get with ETag
	rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns.ID, principal, nil)
	if rec.Code != 200 || rec.Header().Get("ETag") != etag {
		t.Errorf("get = (%d, %q)", rec.Code, rec.Header().Get("ETag"))
	}

	// stale If-Match → 412
	rec = testutil.Do(t, h, http.MethodPatch, base+"/"+ns.ID, staleIfMatch(principal, `"99"`),
		map[string]any{"description": "x"})
	if rec.Code != 412 {
		t.Errorf("stale If-Match: status = %d, want 412", rec.Code)
	}

	// patch with correct If-Match
	rec = testutil.Do(t, h, http.MethodPatch, base+"/"+ns.ID, staleIfMatch(principal, etag),
		map[string]any{"description": "updated"})
	if rec.Code != 200 {
		t.Fatalf("patch: status = %d: %s", rec.Code, rec.Body.String())
	}
	var patched store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched.Description != "updated" || patched.Version != 2 {
		t.Errorf("patched = %+v", patched)
	}

	// idempotent replay returns identical response
	rec1 := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "k3"), map[string]string{"name": "ns2"})
	rec2 := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "k3"), map[string]string{"name": "ns2"})
	if rec1.Code != 201 || rec2.Code != 201 || rec2.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("replay: rec1=%d rec2=%d replayed=%q",
			rec1.Code, rec2.Code, rec2.Header().Get("Idempotency-Replayed"))
	}
	var ns2a, ns2b store.Namespace
	_ = json.Unmarshal(rec1.Body.Bytes(), &ns2a)
	_ = json.Unmarshal(rec2.Body.Bytes(), &ns2b)
	if ns2a.ID != ns2b.ID {
		t.Errorf("replay returned different object: %s vs %s", ns2a.ID, ns2b.ID)
	}

	// same key different payload → 422
	rec = testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "k3"), map[string]string{"name": "ns9"})
	if rec.Code != 422 {
		t.Errorf("key reuse with different payload: status = %d, want 422", rec.Code)
	}

	// delete (DRAFT → DELETING) → 202
	rec = testutil.Do(t, h, http.MethodDelete, base+"/"+ns.ID, principal, nil)
	if rec.Code != 202 {
		t.Fatalf("delete: status = %d: %s", rec.Code, rec.Body.String())
	}

	// after delete: status DELETING (reaper finishes to DELETED), still visible
	rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns.ID, principal, nil)
	if rec.Code != 200 {
		t.Errorf("get after delete: status = %d, want 200", rec.Code)
	}
	var deleted store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &deleted)
	if deleted.Status != "DELETING" {
		t.Errorf("status after delete = %q, want DELETING", deleted.Status)
	}

	// events endpoint still serves the namespace
	rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns.ID+"/events", principal, nil)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte("DELETING")) {
		t.Errorf("events after delete = (%d, %s)", rec.Code, rec.Body.String())
	}
	// delete transition recorded in events table (DRAFT -> DELETING)
	var eventCount int64
	if err := db.Model(&store.NamespaceEvent{}).
		Where("object_id = ? AND to_status = ?", ns.ID, "DELETING").
		Count(&eventCount).Error; err != nil || eventCount != 1 {
		t.Errorf("delete event count = %d, err = %v", eventCount, err)
	}
}

func TestOwnerIsolationAndAdminOverride(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	owner := testutil.TenantUserHeaders(t, db, "user")
	admin := testutil.TenantUserHeaders(t, db, "admin")
	other := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(owner, "ko1"), map[string]string{"name": "owned"})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)

	// other user cannot see it (404 — no existence leak)
	if rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns.ID, other, nil); rec.Code != 404 {
		t.Errorf("cross-owner get: status = %d, want 404", rec.Code)
	}
	// other user does not see it in list either
	rec = testutil.Do(t, h, http.MethodGet, base, other, nil)
	if bytes.Contains(rec.Body.Bytes(), []byte(ns.ID)) {
		t.Error("cross-owner list leaked namespace")
	}
	// admin in a DIFFERENT tenant also cannot (tenant isolation always)
	if rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns.ID, admin, nil); rec.Code != 404 {
		t.Errorf("cross-tenant admin get: status = %d, want 404", rec.Code)
	}

	// admin in the SAME tenant can manage across owners
	adminHeaders := sameTenantAdmin(t, db, owner["X-Tenant-Id"])
	rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns.ID, adminHeaders, nil)
	if rec.Code != 200 {
		t.Errorf("same-tenant admin get: status = %d, want 200", rec.Code)
	}
}
