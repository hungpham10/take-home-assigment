package script_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/script"
	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

// Integration tests: requests go through the REAL server composition
// (server.NewRouter) — tests exercise /api/loadtest/v1/namespaces/{id}/scripts
// exactly as nginx routes them. Skips locally without Postgres, fails in CI.

func testServer(t *testing.T) (http.Handler, *gorm.DB, string) {
	t.Helper()
	db := testutil.TestDB(t)
	dir := t.TempDir()
	h := testutil.NewServer(t, testutil.ServerDeps{DB: db, ScriptsDir: dir, Now: time.Now})
	return h, db, dir
}

// nsFixture creates tenant+user+namespace(READY) and returns principal headers
// (with the namespace id under the test-only "X-Ns-Id" key).
func nsFixture(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	tenantID, userID := testutil.TenantUser(t, db, "user")
	suffix := itoa(time.Now().UnixNano())
	ns := store.Namespace{
		TenantID: tenantID, OwnerUserID: userID, Name: "ns-" + suffix,
		Status: "READY", ReadyForLoadtest: true, Version: 1,
		CleanupPolicy: []byte("{}"),
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatal(err)
	}
	h := testutil.PrincipalHeaders(tenantID, userID, "user")
	h["X-Ns-Id"] = ns.ID // test-only channel
	return h
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestINT_SCR_01_02_08_UploadAndValidationLifecycle(t *testing.T) {
	h, db, dir := testServer(t)
	principal := nsFixture(t, db)
	nsID := principal["X-Ns-Id"]
	base := "/api/loadtest/v1/namespaces/" + nsID + "/scripts"

	good := "export default function() { console.log('hi'); }"
	// missing Idempotency-Key → 422
	rec := testutil.Do(t, h, http.MethodPost, base, principal,
		map[string]string{"name": "s1", "content": good})
	if rec.Code != 422 {
		t.Fatalf("missing key: %d", rec.Code)
	}

	// upload good script (sync validation via nil queue)
	rec = testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "s1"),
		map[string]string{"name": "s1", "content": good})
	if rec.Code != 201 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	var sc store.Script
	_ = json.Unmarshal(rec.Body.Bytes(), &sc)
	if sc.Status != "VALIDATED" {
		t.Errorf("status = %q, want VALIDATED (sync validate)", sc.Status)
	}
	if sc.SizeBytes != len(good) || sc.ContentRef == "" {
		t.Errorf("script = %+v", sc)
	}
	// staged content readable + hash matches
	content, err := script.ReadStaged(dir, sc.ContentRef)
	if err != nil || string(content) != good {
		t.Errorf("staged content = (%q, %v)", content, err)
	}

	// oversized → 422 (DB CHECK too)
	rec = testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "s2"),
		map[string]string{"name": "s2", "content": strings.Repeat("x", script.MaxSize+1)})
	if rec.Code != 422 {
		t.Errorf("oversized: %d, want 422", rec.Code)
	}

	// empty content rejected at handler level
	rec = testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "s3"),
		map[string]string{"name": "s3", "content": ""})
	if rec.Code != 422 {
		t.Errorf("empty upload: %d, want 422", rec.Code)
	}

	// validations recorded
	rec = testutil.Do(t, h, http.MethodGet, base+"/"+sc.ID+"/validations", principal, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "VALIDATED") {
		t.Errorf("validations = (%d, %s)", rec.Code, rec.Body.String())
	}

	// revalidate VALIDATED → VALIDATING → VALIDATED
	rec = testutil.Do(t, h, http.MethodPost, base+"/"+sc.ID+"/validate", principal, nil)
	if rec.Code != 202 {
		t.Fatalf("revalidate: %d %s", rec.Code, rec.Body.String())
	}
}

func TestINT_SCR_07_PathTraversalBlocked(t *testing.T) {
	_, _, dir := testServer(t)
	if _, err := script.ReadStaged(dir, "../../etc/passwd"); err == nil {
		t.Error("path traversal must be rejected")
	}
}
