package adminapi_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/secrets"
	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

// Integration tests: requests go through the REAL server composition
// (server.NewRouter via testutil) — internal endpoints without principal,
// operator endpoints behind authz.RequirePrincipal. Skips locally without
// Postgres, fails in CI.

// masterKey is a fixed 32-byte AES-256-GCM test key.
var masterKey = []byte("0123456789abcdef0123456789abcdef")

// fixtures creates tenant + host(<slug>.local) + user; returns (h, db, tenantID, userID, slug).
func fixtures(t *testing.T) (http.Handler, *gorm.DB, string, string, string) {
	t.Helper()
	db := testutil.TestDB(t)
	h := testutil.NewServer(t, testutil.ServerDeps{
		DB: db, Now: time.Now, Secrets: secrets.New(), MasterKey: masterKey,
	})
	sum := sha256.Sum256([]byte(time.Now().String()))
	slug := "fx" + hex.EncodeToString(sum[:])[:12]
	tenant := store.Tenant{Name: "Fx", Slug: slug, Status: "ACTIVE"}
	if err := db.Create(&tenant).Error; err != nil {
		t.Fatalf("fixtures tenant: %v", err)
	}
	user := store.User{TenantID: tenant.ID, Email: "fx-" + tenant.ID + "@x.co", DisplayName: "Fx"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("fixtures user: %v", err)
	}
	// RequirePrincipalWithRole resolves roles from tbl_user_roles — headers
	// claiming a role without a DB row now fail closed (plan16 API-AUTHZ-04).
	if err := db.Create(&store.UserRole{UserID: user.ID, Role: "user"}).Error; err != nil {
		t.Fatal(err)
	}
	host := store.TenantHost{TenantID: tenant.ID, Host: slug + ".local"}
	if err := db.Where(store.TenantHost{Host: host.Host}).
		Assign(store.TenantHost{TenantID: tenant.ID}).
		FirstOrCreate(&host).Error; err != nil {
		t.Fatal(err)
	}
	return h, db, tenant.ID, user.ID, slug
}

func TestINT_ADM_01_02_ResolveTenantID(t *testing.T) {
	h, _, tenantID, _, slug := fixtures(t)
	host := slug + ".local"

	rec := testutil.Do(t, h, http.MethodGet, "/api/admin/v1/tenant/"+host+"/id", nil, nil)
	if rec.Code != 200 || rec.Body.String() != tenantID {
		t.Errorf("resolve = (%d, %q), want (200, %q)", rec.Code, rec.Body.String(), tenantID)
	}

	rec = testutil.Do(t, h, http.MethodGet, "/api/admin/v1/tenant/unknown.local/id", nil, nil)
	if rec.Code != 404 {
		t.Errorf("unknown host status = %d, want 404", rec.Code)
	}
}

func TestINT_ADM_03_04_05_06_07_AuthConfigShape(t *testing.T) {
	h, db, tenantID, _, slug := fixtures(t)
	cfg := store.AuthProfile{
		TenantID: tenantID,
		Name:     "default",
		Issuer:   store.StrPtr("http://dex:5556/dex"),
		ClientID: store.StrPtr("cid"),
		JwksURI:  store.StrPtr("http://dex:5556/dex/jwks"),
		Config:   store.JSONBytes(map[string]any{"jwt_mode": "jwks"}),
	}
	if err := db.Create(&cfg).Error; err != nil {
		t.Fatal(err)
	}

	// Secrets come from SOPS-decrypted env, NOT the DB (plans/15).
	t.Setenv("LTP_TENANT_"+secrets.SanitizeSlug(slug)+"_OIDC_CLIENT_SECRET", "s3cr3t")
	t.Setenv("LTP_TENANT_"+secrets.SanitizeSlug(slug)+"_SESSION_SECRET", "sess")

	rec := testutil.Do(t, h, http.MethodGet,
		"/api/admin/v1/tenant/"+slug+".local/auth-config?name=default", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("auth-config status = %d", rec.Code)
	}
	var body struct {
		Auth struct {
			ID               string `json:"id"`
			JWTMode          string `json:"jwt_mode"`
			OIDCJWKSURL      string `json:"oidc_jwks_url"`
			OIDCIssuer       string `json:"oidc_issuer"`
			OIDCClientID     string `json:"oidc_client_id"`
			OIDCClientSecret string `json:"oidc_client_secret"`
			OIDCExpectedAlg  string `json:"oidc_expected_alg"`
			SessionSecret    string `json:"session_secret"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	a := body.Auth
	// id phải là TENANT id (nginx set thẳng thành X-Tenant-Id), không phải
	// auth-profile id — xem plans/36 "Fix log" #4.
	if a.ID != tenantID {
		t.Errorf("auth.id = %q, want tenant id %q", a.ID, tenantID)
	}
	if a.OIDCIssuer != "http://dex:5556/dex" || a.OIDCClientID != "cid" ||
		a.OIDCClientSecret != "s3cr3t" || a.SessionSecret != "sess" ||
		a.JWTMode != "jwks" || a.OIDCExpectedAlg != "RS256" {
		t.Errorf("auth config mismatch: %+v", a)
	}
}

type introspectResponse struct {
	Active    bool      `json:"active"`
	UserID    string    `json:"user_id,omitempty"`
	TenantID  string    `json:"tenant_id,omitempty"`
	Roles     []string  `json:"roles,omitempty"`
	IsAdmin   bool      `json:"is_admin"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

func introspect(t *testing.T, h http.Handler, token string) introspectResponse {
	t.Helper()
	rec := testutil.Do(t, h, http.MethodPost, "/api/admin/v1/token/introspect", nil,
		map[string]string{"token": token})
	var resp introspectResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp
}

func TestINT_ADM_08_09_10_11_12_15_TokenLifecycle(t *testing.T) {
	h, db, tenantID, userID, _ := fixtures(t)

	// issue by ANOTHER user in same tenant without admin → 403 (self-only gate)
	other := store.User{TenantID: tenantID, Email: "other-" + tenantID + "@x.co", DisplayName: "O"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.UserRole{UserID: other.ID, Role: "user"}).Error; err != nil {
		t.Fatal(err)
	}
	oheaders := testutil.PrincipalHeaders(tenantID, other.ID, "user")
	rec := testutil.Do(t, h, http.MethodPost, "/api/admin/v1/users/"+userID+"/tokens", oheaders, nil)
	if rec.Code != 403 {
		t.Fatalf("self-only gate: status = %d, want 403", rec.Code)
	}

	// issue — self (plaintext returned exactly once)
	rec = testutil.Do(t, h, http.MethodPost, "/api/admin/v1/users/"+userID+"/tokens",
		testutil.PrincipalHeaders(tenantID, userID, "user"), nil)
	if rec.Code != 201 {
		t.Fatalf("issue: status = %d: %s", rec.Code, rec.Body.String())
	}
	var issued struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &issued)
	if issued.Token == "" {
		t.Fatal("no plaintext token returned")
	}

	// introspect — active, correct identity
	resp := introspect(t, h, issued.Token)
	if !resp.Active || resp.UserID != userID || resp.TenantID != tenantID || resp.IsAdmin {
		t.Errorf("introspect = %+v", resp)
	}

	// tampered token → inactive
	resp = introspect(t, h, issued.Token[:len(issued.Token)-1]+"X")
	if resp.Active {
		t.Error("tampered token must be inactive")
	}

	// reveal by owner — master key was configured at issue time
	rec = testutil.Do(t, h, http.MethodPost, "/api/admin/v1/tokens/"+issued.ID+"/reveal",
		testutil.PrincipalHeaders(tenantID, userID, "user"), nil)
	var revealed struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &revealed)
	if rec.Code != 200 || revealed.Token != issued.Token {
		t.Errorf("reveal = (%d, %q), want (200, %q)", rec.Code, revealed.Token, issued.Token)
	}

	// reveal by the other (non-owner) → 403
	rec = testutil.Do(t, h, http.MethodPost, "/api/admin/v1/tokens/"+issued.ID+"/reveal", oheaders, nil)
	if rec.Code != 403 {
		t.Errorf("reveal self-only gate: status = %d, want 403", rec.Code)
	}

	// revoke by the other (non-owner) user → 403
	rec = testutil.Do(t, h, http.MethodPost, "/api/admin/v1/tokens/"+issued.ID+"/revoke", oheaders, nil)
	if rec.Code != 403 {
		t.Fatalf("revoke self-only gate: status = %d, want 403", rec.Code)
	}

	// revoke by owner → inactive
	rec = testutil.Do(t, h, http.MethodPost, "/api/admin/v1/tokens/"+issued.ID+"/revoke",
		testutil.PrincipalHeaders(tenantID, userID, "user"), nil)
	if rec.Code != 200 {
		t.Fatalf("revoke status = %d", rec.Code)
	}
	resp = introspect(t, h, issued.Token)
	if resp.Active {
		t.Error("revoked token must be inactive")
	}
}

func TestINT_ADM_11_IntrospectExpired(t *testing.T) {
	h, _, tenantID, userID, _ := fixtures(t)

	rec := testutil.Do(t, h, http.MethodPost, "/api/admin/v1/users/"+userID+"/tokens",
		testutil.PrincipalHeaders(tenantID, userID, "user"),
		map[string]string{"ttl": "1ns"})
	var issued struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &issued)

	time.Sleep(2 * time.Millisecond)
	resp := introspect(t, h, issued.Token)
	if resp.Active {
		t.Error("expired token must be inactive")
	}
}

// operator endpoints require the principal middleware (fail-closed).
func TestINT_ADM_16_OperatorRequiresPrincipal(t *testing.T) {
	h, _, _, userID, _ := fixtures(t)
	rec := testutil.Do(t, h, http.MethodPost, fmt.Sprintf("/api/admin/v1/users/%s/tokens", userID), nil, nil)
	if rec.Code != 401 {
		t.Errorf("no-principal issue = %d, want 401", rec.Code)
	}
}
