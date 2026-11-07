package adminapi

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
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Integration tests (opsense-rs style): skip when Postgres is unreachable
// locally; hard-fail in CI when TEST_DATABASE_DSN + CI are set.

func testDeps(t *testing.T) Deps {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://ltp:ltp@localhost:5432/ltp?sslmode=disable"
	}
	ctx := context.Background()
	db, err := store.New(ctx, dsn)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI requires postgres: %v", err)
		}
		t.Skipf("postgres unavailable: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := store.Migrate(ctx, dsn, platform.Migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return Deps{DB: db, Now: func() time.Time { return time.Now() }}
}

// fixtures creates tenant + host(fx.local) + user; returns (tenantID, userID).
func fixtures(t *testing.T, d Deps) (string, string) {
	t.Helper()
	ctx := context.Background()
	tenant := store.Tenant{Name: "Fx", Slug: "fx-" + tokenHash(time.Now().String())[:16], Status: "ACTIVE"}
	if err := d.DB.WithContext(ctx).Create(&tenant).Error; err != nil {
		t.Fatalf("fixtures tenant: %v", err)
	}
	user := store.User{TenantID: tenant.ID, Email: "fx-" + tenant.ID + "@x.co", DisplayName: "Fx"}
	if err := d.DB.WithContext(ctx).Create(&user).Error; err != nil {
		t.Fatalf("fixtures user: %v", err)
	}
	host := store.TenantHost{TenantID: tenant.ID, Host: "fx.local"}
	if err := d.DB.WithContext(ctx).
		Where(store.TenantHost{Host: "fx.local"}).
		Assign(store.TenantHost{TenantID: tenant.ID}).
		FirstOrCreate(&host).Error; err != nil {
		t.Fatal(err)
	}
	return tenant.ID, user.ID
}

func TestResolveTenantID(t *testing.T) {
	d := testDeps(t)
	tenantID, _ := fixtures(t, d)
	h := Router(d)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tenant/fx.local/id", nil))
	if rec.Code != 200 || rec.Body.String() != tenantID {
		t.Errorf("resolve = (%d, %q), want (200, %q)", rec.Code, rec.Body.String(), tenantID)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tenant/unknown.local/id", nil))
	if rec.Code != 404 {
		t.Errorf("unknown host status = %d, want 404", rec.Code)
	}
}

func TestAuthConfigShape(t *testing.T) {
	d := testDeps(t)
	tenantID, _ := fixtures(t, d)
	cfg := store.AuthProfile{
		TenantID: tenantID,
		Name:     "default",
		Issuer:   ptr("http://dex:5556/dex"),
		ClientID: ptr("cid"),
		JwksURI:  ptr("http://dex:5556/dex/jwks"),
		Config:   jsonBytes(map[string]any{"oidc_client_secret": "s3cr3t", "session_secret": "sess"}),
	}
	if err := d.DB.Create(&cfg).Error; err != nil {
		t.Fatal(err)
	}

	h := Router(d)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tenant/fx.local/auth-config?name=default", nil))
	if rec.Code != 200 {
		t.Fatalf("auth-config status = %d", rec.Code)
	}
	var body struct {
		Auth authConfig `json:"auth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	a := body.Auth
	if a.OIDCIssuer != "http://dex:5556/dex" || a.OIDCClientID != "cid" ||
		a.OIDCClientSecret != "s3cr3t" || a.SessionSecret != "sess" ||
		a.JWTMode != "jwks" || a.OIDCExpectedAlg != "RS256" {
		t.Errorf("auth config mismatch: %+v", a)
	}
}

func TestIntrospectLifecycle(t *testing.T) {
	d := testDeps(t)
	tenantID, userID := fixtures(t, d)
	h := Router(d)

	// issue — plaintext returned exactly once
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/users/"+userID+"/tokens", nil))
	if rec.Code != 201 {
		t.Fatalf("issue status = %d: %s", rec.Code, rec.Body.String())
	}
	var issued struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}

	// introspect active (matching tenant header)
	rec = httptest.NewRecorder()
	ireq := httptest.NewRequest(http.MethodPost, "/token/introspect",
		bytes.NewReader(jsonBytes(map[string]string{"token": issued.Token})))
	ireq.Header.Set("X-Tenant-Id", tenantID)
	h.ServeHTTP(rec, ireq)
	var resp introspectResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Active || resp.UserID != userID || resp.TenantID != tenantID || resp.IsAdmin {
		t.Errorf("introspect = %+v", resp)
	}

	// wrong tenant header → inactive
	rec = httptest.NewRecorder()
	ireq = httptest.NewRequest(http.MethodPost, "/token/introspect",
		bytes.NewReader(jsonBytes(map[string]string{"token": issued.Token})))
	ireq.Header.Set("X-Tenant-Id", "00000000-0000-0000-0000-000000000000")
	h.ServeHTTP(rec, ireq)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Active {
		t.Error("cross-tenant introspect must be inactive")
	}

	// tampered token → inactive
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/token/introspect",
		bytes.NewReader(jsonBytes(map[string]string{"token": issued.Token[:len(issued.Token)-1] + "X"}))))
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Active {
		t.Error("tampered token must be inactive")
	}

	// revoke → inactive
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tokens/"+issued.ID+"/revoke", nil))
	if rec.Code != 200 {
		t.Fatalf("revoke status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/token/introspect",
		bytes.NewReader(jsonBytes(map[string]string{"token": issued.Token}))))
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Active {
		t.Error("revoked token must be inactive")
	}
}

func TestIntrospectExpired(t *testing.T) {
	d := testDeps(t)
	_, userID := fixtures(t, d)
	h := Router(d)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/users/"+userID+"/tokens",
		bytes.NewReader(jsonBytes(map[string]string{"ttl": "1ns"}))))
	var issued struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &issued)

	time.Sleep(2 * time.Millisecond)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/token/introspect",
		bytes.NewReader(jsonBytes(map[string]string{"token": issued.Token}))))
	var resp introspectResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Active {
		t.Error("expired token must be inactive")
	}
}

func ptr(s string) *string { return &s }
