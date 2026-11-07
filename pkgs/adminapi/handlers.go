// Package adminapi implements /api/admin/v1 — the control-plane account/
// tenant/auth API consumed by conf/nginx (OpenResty/Lua) and operators.
//
// nginx integration endpoints (docs/admin-oidc-integration-api.md §3):
//   - GET  /tenant/{host}/id          → plain tenant UUID (Lua read_body → header)
//   - GET  /tenant/{host}/auth-config → {"auth":{...}} shape consumed by Lua
//   - POST /token/introspect          → {"active":...} for abt_ base tokens
package adminapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/httpx"
)

// Deps carries the handlers' dependencies.
type Deps struct {
	DB  *gorm.DB
	Now func() time.Time // injectable clock for tests
}

// Router mounts the admin API routes on a chi router.
func Router(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Get("/tenant/{host}/id", d.resolveTenantID)
	r.Get("/tenant/{host}/auth-config", d.authConfig)
	r.Post("/token/introspect", d.introspect)
	r.Post("/users/{userID}/tokens", d.issueToken)
	r.Post("/tokens/{tokenID}/revoke", d.revokeToken)
	return r
}

// --- tenant id ---------------------------------------------------------------

func (d Deps) resolveTenantID(w http.ResponseWriter, r *http.Request) {
	host := chi.URLParam(r, "host")
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var id string
	err := d.DB.WithContext(ctx).Raw(`
		SELECT t.id::text FROM tbl_tenants t
		JOIN tbl_tenant_hosts h ON h.tenant_id = t.id
		WHERE h.host = ?`, host).Scan(&id).Error
	if err != nil || id == "" {
		// nginx treats 404 as tenant resolution failure (error path).
		httpx.WriteError(w, http.StatusNotFound, "OBJECT_NOT_FOUND", "no tenant for host "+host)
		return
	}
	// Plain body — conf/nginx reads res:read_body() straight into X-Tenant-Id.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(id))
}

// --- auth config --------------------------------------------------------------

type authConfig struct {
	ID               string `json:"id"`
	JWTMode          string `json:"jwt_mode"`
	OIDCJWKSURL      string `json:"oidc_jwks_url"`
	JWTSecret        string `json:"jwt_secret"`
	OIDCIssuer       string `json:"oidc_issuer"`
	OIDCClientID     string `json:"oidc_client_id"`
	OIDCClientSecret string `json:"oidc_client_secret"`
	OIDCExpectedAlg  string `json:"oidc_expected_alg"`
	SessionSecret    string `json:"session_secret"`
}

type authConfigBody struct {
	Auth authConfig `json:"auth"`
}

type authProfileRow struct {
	ID       string
	Issuer   *string
	ClientID *string        `gorm:"column:client_id"`
	JwksURI  *string        `gorm:"column:jwks_uri"`
	Config   map[string]any `gorm:"column:config"`
}

func (d Deps) authConfig(w http.ResponseWriter, r *http.Request) {
	host := chi.URLParam(r, "host")
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "default"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var row authProfileRow
	err := d.DB.WithContext(ctx).Raw(`
		SELECT p.id::text, p.issuer, p.client_id, p.jwks_uri, p.config
		FROM tbl_tenant_auth_profiles p
		JOIN tbl_tenant_hosts h ON h.tenant_id = p.tenant_id
		WHERE h.host = ? AND p.name = ?`, host, name).Scan(&row).Error
	if err != nil || row.ID == "" {
		httpx.WriteError(w, http.StatusNotFound, "OBJECT_NOT_FOUND",
			fmt.Sprintf("not found host %s for oidc %s", host, name))
		return
	}

	cfg := authConfig{
		ID:               row.ID,
		JWTMode:          str(row.Config, "jwt_mode", "jwks"),
		OIDCJWKSURL:      deref(row.JwksURI),
		JWTSecret:        str(row.Config, "jwt_secret", ""),
		OIDCIssuer:       deref(row.Issuer),
		OIDCClientID:     deref(row.ClientID),
		OIDCClientSecret: str(row.Config, "oidc_client_secret", ""),
		OIDCExpectedAlg:  str(row.Config, "oidc_expected_alg", "RS256"),
		SessionSecret:    str(row.Config, "session_secret", ""),
	}
	// SECURITY: contains secrets — trusted nginx channel only, never log.
	httpx.WriteJSON(w, http.StatusOK, authConfigBody{Auth: cfg})
}

// --- token introspection -------------------------------------------------------

type introspectRequest struct {
	Token string `json:"token"`
}

type introspectResponse struct {
	Active    bool      `json:"active"`
	UserID    string    `json:"user_id,omitempty"`
	TenantID  string    `json:"tenant_id,omitempty"`
	Roles     []string  `json:"roles,omitempty"`
	IsAdmin   bool      `json:"is_admin"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

func (d Deps) introspect(w http.ResponseWriter, r *http.Request) {
	var req introspectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		httpx.WriteJSON(w, http.StatusOK, introspectResponse{Active: false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	resp := d.introspectToken(ctx, req.Token, r.Header.Get("X-Tenant-Id"))
	httpx.WriteJSON(w, http.StatusOK, resp)
}

type tokenLookupRow struct {
	TokenHash string
	UserID    string
	TenantID  string
	Role      *string
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

// introspectToken performs hashed prefix lookup + constant-time hash compare.
func (d Deps) introspectToken(ctx context.Context, token, tenantID string) introspectResponse {
	const prefixLen = 8
	if len(token) <= prefixLen {
		return introspectResponse{}
	}
	prefix, want := token[:prefixLen], tokenHash(token)

	var row tokenLookupRow
	err := d.DB.WithContext(ctx).Raw(`
		SELECT at.token_hash, at.user_id::text, u.tenant_id::text,
		       (SELECT role FROM tbl_user_roles r WHERE r.user_id = u.id LIMIT 1),
		       at.expires_at, at.revoked_at
		FROM tbl_access_tokens at
		JOIN tbl_users u ON u.id = at.user_id
		WHERE at.prefix = ?`, prefix).Scan(&row).Error
	if err != nil || row.TokenHash == "" {
		return introspectResponse{}
	}
	// Constant-time compare; never short-circuit on hash mismatch ordering.
	if subtle.ConstantTimeCompare([]byte(row.TokenHash), []byte(want)) != 1 {
		return introspectResponse{}
	}
	now := d.now()
	if row.RevokedAt != nil || (row.ExpiresAt != nil && !row.ExpiresAt.After(now)) {
		return introspectResponse{}
	}
	if tenantID != "" && tenantID != row.TenantID {
		return introspectResponse{}
	}
	role := deref(row.Role)
	return introspectResponse{
		Active:    true,
		UserID:    row.UserID,
		TenantID:  row.TenantID,
		Roles:     []string{role},
		IsAdmin:   role == "admin",
		ExpiresAt: derefTime(row.ExpiresAt),
	}
}

// --- token issue / revoke -------------------------------------------------------

func (d Deps) issueToken(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")
	var body struct {
		Name string `json:"name"`
		TTL  string `json:"ttl"`
	}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				httpx.WriteCodeError(w, "BAD_REQUEST", "invalid JSON body")
				return
			}
		}
	}
	ttl := 24 * 30 * time.Hour
	if body.TTL != "" {
		dur, err := time.ParseDuration(body.TTL)
		if err != nil || dur <= 0 {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "invalid ttl duration")
			return
		}
		ttl = dur
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	secret, err := newToken()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "token generation failed")
		return
	}
	expires := d.now().Add(ttl)
	var id string
	err = d.DB.WithContext(ctx).Raw(`
		INSERT INTO tbl_access_tokens (user_id, name, token_hash, prefix, expires_at)
		VALUES (?::uuid, ?, ?, ?, ?) RETURNING id::text`,
		userID, body.Name, tokenHash("abt_"+secret), tokenPrefix("abt_"+secret), expires).Scan(&id).Error
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "token insert failed")
		return
	}
	d.audit(ctx, r, "token.issue", "access_token", id)
	// Plaintext returned exactly once; only the hash is stored.
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":         id,
		"token":      "abt_" + secret,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

func (d Deps) revokeToken(w http.ResponseWriter, r *http.Request) {
	tokenID := chi.URLParam(r, "tokenID")
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	res := d.DB.WithContext(ctx).Exec(
		`UPDATE tbl_access_tokens SET revoked_at = now()
		 WHERE id = ?::uuid AND revoked_at IS NULL`, tokenID)
	if res.Error != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "revoke failed")
		return
	}
	if res.RowsAffected == 0 {
		httpx.WriteError(w, http.StatusNotFound, "OBJECT_NOT_FOUND", "token not found or already revoked")
		return
	}
	d.audit(ctx, r, "token.revoke", "access_token", tokenID)
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

// --- helpers ---------------------------------------------------------------------

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) audit(ctx context.Context, r *http.Request, action, objectType, objectID string) {
	_ = d.DB.WithContext(ctx).Exec(`
		INSERT INTO tbl_audit_log (tenant_id, actor, action, object_type, object_id)
		VALUES (NULLIF(?,'')::uuid, ?::jsonb, ?, ?, ?)`,
		r.Header.Get("X-Tenant-Id"),
		actorJSON(r.Header.Get("X-User-Id")),
		action, objectType, objectID).Error
}

func actorJSON(userID string) string {
	b, _ := json.Marshal(map[string]string{"user_id": userID})
	return string(b)
}

func str(m map[string]any, key, def string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return def
}

// tokenPrefix returns the lookup prefix (first 8 chars of the full token).
func tokenPrefix(token string) string { return token[:8] }

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
