// Package authz builds the canonical principal from nginx-injected trusted
// headers and enforces fail-closed RBAC.
//
// Trust boundary: X-Tenant-Id / X-User-Id / X-User-Role ... are trusted ONLY
// at the nginx→backend boundary (unix socket / private upstream). nginx must
// strip/overwrite them from client requests.
package authz

import (
	"context"
	"net/http"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/httpx"
)

// Headers injected by conf/nginx (vhost/04-api.conf).
const (
	headerTenantID = "X-Tenant-Id"
	headerUserID   = "X-User-Id"
	headerRole     = "X-User-Role"
)

type ctxKey struct{}

// Principal is the canonical identity for one request.
type Principal struct {
	TenantID string // always applied — tenant isolation boundary
	UserID   string // authenticated subject; "public" on explicitly public routes
	Role     string // "admin" | "user" | "" (unknown)
}

// IsAdmin reports whether the principal carries an admin role.
// Canonical normalization: X-User-Role=admin OR role resolved server-side
// (introspection). Never derived from client payload.
func (p Principal) IsAdmin() bool { return p.Role == "admin" }

// WithContext returns ctx carrying p.
func (p Principal) WithContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext extracts the Principal. Returns ok=false when absent
// (e.g. public route that skipped the middleware).
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// RequirePrincipal is fail-closed middleware: protected routes need both
// X-Tenant-Id and X-User-Id; missing → 401 (per docs/admin-oidc-integration-api.md §2.3).
func RequirePrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := Principal{
			TenantID: r.Header.Get(headerTenantID),
			UserID:   r.Header.Get(headerUserID),
			Role:     r.Header.Get(headerRole),
		}
		if p.TenantID == "" || p.UserID == "" {
			httpx.WriteCodeError(w, "UNAUTHORIZED", "missing tenant/user identity headers")
			return
		}
		next.ServeHTTP(w, r.WithContext(p.WithContext(r.Context())))
	})
}

// PrincipalOf returns the principal from the request context (must be used
// downstream of RequirePrincipal).
func PrincipalOf(r *http.Request) Principal {
	p, _ := FromContext(r.Context())
	return p
}

// RequirePrincipalWithRole wraps RequirePrincipal, resolves an opaque external
// subject (OIDC `sub`, plan 37) to a local user id, and resolves the role
// server-side from tbl_user_roles/tbl_users (plan16 API-AUTHZ-04 — the
// X-User-Role header alone is not authoritative):
//
//   - identity not resolvable → fail-closed 401.
//   - DB role found → DB role wins regardless of the header claim.
//   - no DB role + empty header  → proceed with an empty role (unprivileged).
//   - no DB role + header claims a role → fail-closed 401 (spoofed/unprovisioned identity).
func RequirePrincipalWithRole(db *gorm.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return RequirePrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalOf(r)
			userID, err := resolveIdentity(db, p.TenantID, p.UserID)
			if err != nil {
				httpx.WriteCodeError(w, "UNAUTHORIZED", "identity not recognized")
				return
			}
			p.UserID = userID

			var role string
			err = db.WithContext(r.Context()).Raw(
				`SELECT ur.role FROM tbl_user_roles ur
				 JOIN tbl_users u ON u.id = ur.user_id
				 WHERE u.id = ?::uuid AND u.tenant_id = ?::uuid
				 LIMIT 1`, p.UserID, p.TenantID).Scan(&role).Error
			if err == nil && role != "" {
				p.Role = role
				next.ServeHTTP(w, r.WithContext(p.WithContext(r.Context())))
				return
			}
			if p.Role == "" {
				next.ServeHTTP(w, r.WithContext(p.WithContext(r.Context())))
				return
			}
			httpx.WriteCodeError(w, "UNAUTHORIZED", "identity not recognized")
		}))
	}
}
