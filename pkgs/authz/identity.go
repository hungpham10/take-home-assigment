// Identity resolution at the trusted nginx→backend boundary (plan 37).
//
// nginx forwards the authenticated subject as X-User-Id. Two shapes reach us:
//
//   - a local UUID (legacy header mode, `abt_` access tokens, internal callers), and
//   - the raw OIDC `sub`, which is ALWAYS opaque: Dex v2.41 emits
//     base64url(protobuf{userID, connectorID}) with no way to disable it
//     (server/oauth2.go:genSubject).
//
// Local rows (tbl_namespaces.owner_user_id) require tbl_users.id, so an opaque
// subject has to be mapped through tbl_user_external_identities — the schema's
// bridge between IdP subjects and local users. Unmapped subjects fail closed:
// provisioning is an admin concern, never implicit.
package authz

import (
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrUnknownIdentity is returned when X-User-Id is neither a UUID nor a subject
// present in tbl_user_external_identities for the request's tenant.
var ErrUnknownIdentity = errors.New("authz: unknown identity")

// resolveIdentity returns the local tbl_users.id for the header value.
//
// UUID values pass through unchanged (existence/tenant match is left to the
// downstream queries, preserving pre-plan-37 behavior). Anything else is
// treated as an external subject and looked up tenant-scoped.
func resolveIdentity(db *gorm.DB, tenantID, userID string) (string, error) {
	if _, err := uuid.Parse(userID); err == nil {
		return userID, nil
	}

	var resolved string
	err := db.Raw(
		`SELECT u.id
		   FROM tbl_user_external_identities uei
		   JOIN tbl_users u ON u.id = uei.user_id
		  WHERE uei.subject = ? AND u.tenant_id = ?::uuid
		  LIMIT 1`, userID, tenantID).Scan(&resolved).Error
	if err != nil {
		return "", err
	}
	if resolved == "" {
		return "", ErrUnknownIdentity
	}
	return resolved, nil
}
