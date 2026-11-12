package authz

import (
	"errors"
)

// ErrForbidden is returned when the caller is neither the resource owner nor
// an admin (maps to HTTP 403).
var ErrForbidden = errors.New("authz: forbidden")

// AdminRole is the only role that bypasses the self-only gate.
// Pattern from opsense-rs `require_self_or_admin`: keep the trusted role
// source explicit; X-User-Role is trusted only at the nginx→backend boundary
// and the role must also exist in tbl_user_roles (server-side resolution).
const AdminRole = "admin"

// RequireSelfOrAdmin enforces the self-only gate: the caller may operate on
// their own resources; an admin may operate across owners WITHIN the same
// tenant only (docs/admin-oidc-integration-api.md §2.3).
//
// p comes from the request context (RequirePrincipal); targetTenantID/
// targetUserID identify the resource being accessed.
func RequireSelfOrAdmin(p Principal, targetTenantID, targetUserID string) error {
	if p.TenantID != targetTenantID {
		// Tenant isolation always applies — even for admins.
		return ErrForbidden
	}
	if p.UserID == targetUserID {
		return nil
	}
	if p.IsAdmin() {
		return nil
	}
	return ErrForbidden
}
