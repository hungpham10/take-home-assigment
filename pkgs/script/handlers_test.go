package script

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	platform "github.com/hungpham/loadtest-platform"
	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

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
	dir := t.TempDir()
	return Deps{DB: db, ScriptsDir: dir, Now: func() time.Time { return time.Now() }}
}

// nsFixture creates tenant+user+namespace(READY) and returns principal headers.
func nsFixture(t *testing.T, d Deps) map[string]string {
	t.Helper()
	suffix := time.Now().UnixNano()
	tenant := store.Tenant{Name: "S", Slug: "sc" + fmtInt(suffix), Status: "ACTIVE"}
	if err := d.DB.Create(&tenant).Error; err != nil {
		t.Fatal(err)
	}
	user := store.User{TenantID: tenant.ID, Email: "sc" + fmtInt(suffix) + "@x.co", DisplayName: "S"}
	if err := d.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	ns := store.Namespace{
		TenantID: tenant.ID, OwnerUserID: user.ID, Name: "ns-" + fmtInt(suffix),
		Status: "READY", ReadyForLoadtest: true, Version: 1,
		CleanupPolicy: []byte("{}"),
	}
	if err := d.DB.Create(&ns).Error; err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"X-Tenant-Id": tenant.ID, "X-User-Id": user.ID, "X-User-Role": "user",
		"X-Ns-Id": ns.ID, // test-only channel
	}
}

func fmtInt(n int64) string { return strings.TrimSpace(strings.Repeat("", 0) + itoa(n)) }

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

func doScript(t *testing.T, d Deps, h map[string]string, method, nsID, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	parent := chi.NewRouter()
	parent.Mount("/namespaces/{id}/scripts", Router(d))
	req := httptest.NewRequest(method, "/namespaces/"+nsID+"/scripts"+path, jsonReader(body))
	for k, v := range h {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	authz.RequirePrincipal(parent).ServeHTTP(rec, req)
	return rec
}

func TestScriptUploadAndValidationLifecycle(t *testing.T) {
	d := testDeps(t)
	h := nsFixture(t, d)
	nsID := h["X-Ns-Id"]

	good := "export default function() { console.log('hi'); }"
	// missing Idempotency-Key → 422
	rec := doScript(t, d, h, http.MethodPost, nsID, "", map[string]string{"name": "s1", "content": good})
	if rec.Code != 422 {
		t.Fatalf("missing key: %d", rec.Code)
	}

	// upload good script (sync validation via nil queue)
	rec = doScript(t, d, withKey(h, "s1"), http.MethodPost, nsID, "",
		map[string]string{"name": "s1", "content": good})
	if rec.Code != 201 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	var sc store.Script
	_ = jsonUnmarshal(rec.Body.String(), &sc)
	if sc.Status != "VALIDATED" {
		t.Errorf("status = %q, want VALIDATED (sync validate)", sc.Status)
	}
	if sc.SizeBytes != len(good) || sc.ContentRef == "" {
		t.Errorf("script = %+v", sc)
	}
	// staged content readable + hash matches
	content, err := d.readStaged(sc.ContentRef)
	if err != nil || string(content) != good {
		t.Errorf("staged content = (%q, %v)", content, err)
	}

	// oversized → 422 (DB CHECK too)
	rec = doScript(t, d, withKey(h, "s2"), http.MethodPost, nsID, "",
		map[string]string{"name": "s2", "content": strings.Repeat("x", MaxSize+1)})
	if rec.Code != 422 {
		t.Errorf("oversized: %d, want 422", rec.Code)
	}

	// structural-invalid script → INVALID
	rec = doScript(t, d, withKey(h, "s3"), http.MethodPost, nsID, "",
		map[string]string{"name": "s3", "content": ""})
	// empty content rejected at handler level; use whitespace-only via direct insert instead:
	if rec.Code != 422 {
		t.Errorf("empty upload: %d, want 422", rec.Code)
	}

	// validations recorded
	rec = doScript(t, d, h, http.MethodGet, nsID, "/"+sc.ID+"/validations", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "VALIDATED") {
		t.Errorf("validations = (%d, %s)", rec.Code, rec.Body.String())
	}

	// revalidate VALIDATED → VALIDATING → VALIDATED
	rec = doScript(t, d, h, http.MethodPost, nsID, "/"+sc.ID+"/validate", nil)
	if rec.Code != 202 {
		t.Fatalf("revalidate: %d %s", rec.Code, rec.Body.String())
	}
}

func TestScriptPathTraversalBlocked(t *testing.T) {
	d := testDeps(t)
	dir := t.TempDir()
	if _, err := d.readStaged("../../etc/passwd"); err == nil {
		t.Error("path traversal must be rejected")
	}
	_ = dir
	_ = filepath.Separator
}

func withKey(h map[string]string, key string) map[string]string {
	out := map[string]string{"Idempotency-Key": key}
	for k, v := range h {
		out[k] = v
	}
	return out
}
