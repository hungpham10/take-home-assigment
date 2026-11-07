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
	"log/slog"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/secrets"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Deps carries the handlers' dependencies.
type Deps struct {
	DB      *gorm.DB
	Now     func() time.Time // injectable clock for tests
	Secrets secrets.Resolver // SOPS-decrypted env secrets (never from DB)
}

// Router mounts the nginx-internal endpoints (trusted channel: unix socket /
// private upstream — no principal middleware; nginx calls carry no user
// identity) plus the operator endpoints behind RequirePrincipal +
// RequireSelfOrAdmin (pattern from opsense-rs require_self_or_admin: the app
// must gate "self-only" itself, never trust the header blindly).
func Router(d Deps) http.Handler {
	r := chi.NewRouter()

	// nginx-internal (security-sensitive, trusted channel only)
	r.Get("/tenant/{host}/id", d.resolveTenantID)
	r.Get("/tenant/{host}/auth-config", d.authConfig)
	r.Post("/token/introspect", d.introspect)

	// operator endpoints — fail-closed principal + self-or-admin gate
	r.Route("/users", func(r chi.Router) {
		r.Use(authz.RequirePrincipal)
		r.Post("/{userID}/tokens", d.issueToken)
		r.Get("/{userID}/tokens", d.listTokens)
	})
	r.Route("/tokens", func(r chi.Router) {
		r.Use(authz.RequirePrincipal)
		r.Post("/{tokenID}/revoke", d.revokeToken)
	})
	return r
}

// --- tenant id ---------------------------------------------------------------

func (d Deps) resolveTenantID(w http.ResponseWriter, r *http.Request) {
	host := chi.URLParam(r, "host")
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var tenant store.Tenant
	err := d.DB.WithContext(ctx).
		Joins("JOIN tbl_tenant_hosts h ON h.tenant_id = tbl_tenants.id").
		Where("h.host = ?", host).
		First(&tenant).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// nginx treats 404 as tenant resolution failure (error path).
		httpx.WriteError(w, http.StatusNotFound, "OBJECT_NOT_FOUND", "no tenant for host "+host)
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "tenant lookup failed")
		return
	}
	// Plain body — conf/nginx reads res:read_body() straight into X-Tenant-Id.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(tenant.ID))
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

func (d Deps) authConfig(w http.ResponseWriter, r *http.Request) {
	host := chi.URLParam(r, "host")
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "default"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var profile store.AuthProfile
	err := d.DB.WithContext(ctx).
		Joins("JOIN tbl_tenant_hosts h ON h.tenant_id = tbl_tenant_auth_profiles.tenant_id").
		Where("h.host = ? AND tbl_tenant_auth_profiles.name = ?", host, name).
		First(&profile).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "OBJECT_NOT_FOUND",
			fmt.Sprintf("not found host %s for oidc %s", host, name))
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "auth config lookup failed")
		return
	}

	// Secrets come from SOPS-decrypted env (LTP_TENANT_<SLUG>_*), never from
	// the DB — tbl_tenant_auth_profiles.config holds non-sensitive fields only
	// (migration 0007 strips legacy values).
	var tenant store.Tenant
	if err := d.DB.WithContext(ctx).First(&tenant, "id = ?", profile.TenantID).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "tenant lookup failed")
		return
	}
	cfg := authConfig{
		ID:               profile.ID,
		JWTMode:          str(profile.Config, "jwt_mode", "jwks"),
		OIDCJWKSURL:      deref(profile.JwksURI),
		JWTSecret:        d.Secrets.Resolve(tenant.Slug, secrets.JWTSecret),
		OIDCIssuer:       deref(profile.Issuer),
		OIDCClientID:     deref(profile.ClientID),
		OIDCClientSecret: d.Secrets.Resolve(tenant.Slug, secrets.OIDCClientSecret),
		OIDCExpectedAlg:  str(profile.Config, "oidc_expected_alg", "RS256"),
		SessionSecret:    d.Secrets.Resolve(tenant.Slug, secrets.SessionSecret),
	}
	if cfg.SessionSecret == "" || cfg.OIDCClientSecret == "" {
		// Warn WITHOUT values (nginx Lua falls back to env vars on its side).
		slog.Warn("auth-config missing tenant secrets from env",
			"tenant", tenant.Slug, "host", host, "profile", name)
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

// introspectToken performs hashed prefix lookup + constant-time hash compare.
func (d Deps) introspectToken(ctx context.Context, token, tenantID string) introspectResponse {
	const prefixLen = 8
	if len(token) <= prefixLen {
		return introspectResponse{}
	}
	prefix, want := token[:prefixLen], tokenHash(token)

	var at store.AccessToken
	err := d.DB.WithContext(ctx).
		Where("prefix = ?", prefix).
		First(&at).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return introspectResponse{}
	}
	if err != nil {
		return introspectResponse{}
	}
	// Constant-time compare; never short-circuit on hash mismatch ordering.
	if subtle.ConstantTimeCompare([]byte(at.TokenHash), []byte(want)) != 1 {
		return introspectResponse{}
	}
	var user store.User
	if err := d.DB.WithContext(ctx).First(&user, "id = ?", at.UserID).Error; err != nil {
		return introspectResponse{}
	}
	var role store.UserRole
	roleErr := d.DB.WithContext(ctx).First(&role, "user_id = ?", at.UserID).Error
	roleName := "user"
	if roleErr == nil {
		roleName = role.Role
	}

	now := d.now()
	if at.RevokedAt != nil || (at.ExpiresAt != nil && !at.ExpiresAt.After(now)) {
		return introspectResponse{}
	}
	if tenantID != "" && tenantID != user.TenantID {
		return introspectResponse{}
	}
	return introspectResponse{
		Active:    true,
		UserID:    user.ID,
		TenantID:  user.TenantID,
		Roles:     []string{roleName},
		IsAdmin:   roleName == "admin",
		ExpiresAt: derefTime(at.ExpiresAt),
	}
}

// --- token issue / revoke -------------------------------------------------------

func (d Deps) issueToken(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")
	if err := authz.RequireSelfOrAdmin(authz.PrincipalOf(r), r.Header.Get("X-Tenant-Id"), userID); err != nil {
		httpx.WriteCodeError(w, "FORBIDDEN", "not allowed to issue tokens for another user")
		return
	}
	var body struct {
		Name string `json:"name"`
		TTL  string `json:"ttl"`
	}
	if r.Body != nil {
		raw, _ := readAll(r.Body)
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
	full := "abt_" + secret
	expires := d.now().Add(ttl)
	at := store.AccessToken{
		UserID:    userID,
		Name:      body.Name,
		TokenHash: tokenHash(full),
		Prefix:    tokenPrefix(full),
		ExpiresAt: &expires,
	}
	if err := d.DB.WithContext(ctx).Create(&at).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "token insert failed")
		return
	}
	d.audit(ctx, r, "token.issue", "access_token", at.ID)
	// Plaintext returned exactly once; only the hash is stored.
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":         at.ID,
		"token":      full,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

func (d Deps) revokeToken(w http.ResponseWriter, r *http.Request) {
	tokenID := chi.URLParam(r, "tokenID")
	if !d.ownsToken(r, tokenID) {
		httpx.WriteCodeError(w, "FORBIDDEN", "not allowed to revoke this token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	now := d.now()
	res := d.DB.WithContext(ctx).
		Model(&store.AccessToken{}).
		Where("id = ? AND revoked_at IS NULL", tokenID).
		Update("revoked_at", &now)
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

// listTokens returns a user's tokens (metadata only — never plaintext).
func (d Deps) listTokens(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")
	if err := authz.RequireSelfOrAdmin(authz.PrincipalOf(r), r.Header.Get("X-Tenant-Id"), userID); err != nil {
		httpx.WriteCodeError(w, "FORBIDDEN", "not allowed to list tokens of another user")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var tokens []store.AccessToken
	if err := d.DB.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Find(&tokens).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "token list failed")
		return
	}
	type tokenInfo struct {
		ID        string     `json:"id"`
		Name      string     `json:"name"`
		Prefix    string     `json:"prefix"`
		ExpiresAt *time.Time `json:"expires_at"`
		RevokedAt *time.Time `json:"revoked_at"`
	}
	out := make([]tokenInfo, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, tokenInfo{ID: t.ID, Name: t.Name, Prefix: t.Prefix, ExpiresAt: t.ExpiresAt, RevokedAt: t.RevokedAt})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// ownsToken checks the caller may revoke tokenID (self-or-admin of the
// token's owner).
func (d Deps) ownsToken(r *http.Request, tokenID string) bool {
	p := authz.PrincipalOf(r)
	var at store.AccessToken
	if err := d.DB.First(&at, "id = ?", tokenID).Error; err != nil {
		return false
	}
	var user store.User
	if err := d.DB.First(&user, "id = ?", at.UserID).Error; err != nil {
		return false
	}
	return authz.RequireSelfOrAdmin(p, user.TenantID, at.UserID) == nil
}

// --- helpers ---------------------------------------------------------------------

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) audit(ctx context.Context, r *http.Request, action, objectType, objectID string) {
	tenantID := r.Header.Get("X-Tenant-Id")
	entry := store.AuditLog{
		Actor:      jsonBytes(map[string]string{"user_id": r.Header.Get("X-User-Id")}),
		Action:     action,
		ObjectType: objectType,
		ObjectID:   &objectID,
	}
	if tenantID != "" {
		entry.TenantID = &tenantID
	}
	_ = d.DB.WithContext(ctx).Create(&entry).Error
}

func str(m json.RawMessage, key, def string) string {
	if len(m) == 0 {
		return def
	}
	var kv map[string]any
	if err := json.Unmarshal(m, &kv); err != nil {
		return def
	}
	if v, ok := kv[key].(string); ok && v != "" {
		return v
	}
	return def
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// tokenPrefix returns the lookup prefix (first 8 chars of the full token).
func tokenPrefix(token string) string { return token[:8] }

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
