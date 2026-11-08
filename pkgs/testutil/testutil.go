// Package testutil provides shared test helpers: a migrated test database,
// the REAL server composition (server.NewRouter incl. authz.RequirePrincipal)
// and request/fixture helpers. Only _test.go files may import this package.
package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	platform "github.com/hungpham/loadtest-platform"
	"github.com/hungpham/loadtest-platform/pkgs/runner"
	"github.com/hungpham/loadtest-platform/pkgs/script"
	"github.com/hungpham/loadtest-platform/pkgs/secrets"
	"github.com/hungpham/loadtest-platform/pkgs/server"
	"github.com/hungpham/loadtest-platform/pkgs/store"

	"gorm.io/gorm"
)

// TestDB connects to Postgres (TEST_DATABASE_DSN or the local default) and
// runs migrations. Skips locally when the DB is unreachable; hard-fails in CI.
func TestDB(t *testing.T) *gorm.DB {
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
	return db
}

// ServerDeps carries everything the test server needs (subset of server.Deps).
type ServerDeps struct {
	DB         *gorm.DB
	Now        func() time.Time
	Secrets    secrets.Resolver // SOPS-decrypted tenant secrets
	MasterKey  []byte           // AES-256-GCM master key (token encryption at rest)
	ScriptsDir string           // staging dir for k6 scripts
	ScriptQ    *script.Queue    // nil → sync validation (tests)
	Runner     runner.Runner    // load-test execution backend
	K8S        bool             // cluster wired → async apply (plans/21)
	StaleAfter time.Duration    // watcher heartbeat staleness threshold (WATCH-05)
}

// NewServer builds the REAL server composition (health/ready/metrics +
// /api/loadtest/v1 with RequirePrincipal + /api/admin/v1 with the operator
// group gated by RequirePrincipal).
func NewServer(t *testing.T, d ServerDeps) http.Handler {
	t.Helper()
	return server.NewRouter(server.Deps{
		DB:         d.DB,
		Now:        d.Now,
		Secrets:    d.Secrets,
		MasterKey:  d.MasterKey,
		ScriptsDir: d.ScriptsDir,
		ScriptQ:    d.ScriptQ,
		Runner:     d.Runner,
		K8S:        d.K8S,
		StaleAfter: d.StaleAfter,
	})
}

// Do performs an HTTP request against handler and returns the recorder.
// body is JSON-marshaled when non-nil.
func Do(t *testing.T, handler http.Handler, method, path string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// WithKey returns headers plus the Idempotency-Key header.
func WithKey(headers map[string]string, key string) map[string]string {
	out := make(map[string]string, len(headers)+1)
	out["Idempotency-Key"] = key
	for k, v := range headers {
		out[k] = v
	}
	return out
}

// PrincipalHeaders returns the nginx-injected trusted principal headers.
func PrincipalHeaders(tenantID, userID, role string) map[string]string {
	return map[string]string{"X-Tenant-Id": tenantID, "X-User-Id": userID, "X-User-Role": role}
}

// TenantUser creates a tenant (ACTIVE, unique slug) + user (+role) and
// returns (tenantID, userID).
func TenantUser(t *testing.T, db *gorm.DB, role string) (string, string) {
	t.Helper()
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	tenant := store.Tenant{Name: "T" + suffix, Slug: "t-" + suffix, Status: "ACTIVE"}
	if err := db.Create(&tenant).Error; err != nil {
		t.Fatalf("TenantUser tenant: %v", err)
	}
	user := store.User{TenantID: tenant.ID, Email: "u-" + suffix + "@x.co", DisplayName: "T"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("TenantUser user: %v", err)
	}
	if role != "" {
		if err := db.Create(&store.UserRole{UserID: user.ID, Role: role}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return tenant.ID, user.ID
}

// TenantUserHeaders creates the tenant+user and returns principal headers.
func TenantUserHeaders(t *testing.T, db *gorm.DB, role string) map[string]string {
	t.Helper()
	tenantID, userID := TenantUser(t, db, role)
	return PrincipalHeaders(tenantID, userID, role)
}
