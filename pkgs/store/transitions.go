package store

import (
	"context"
	"encoding/json"

	"gorm.io/gorm"
)

// Transition moves a state-machine object to `to` via its DB function
// fn_<object>_transition (plans/04 pattern: FOR UPDATE lock → transition map
// → status+version update → event insert, all in the caller's transaction).
//
// This is the ONLY place raw SQL for transitions lives — GORM has no syntax
// for function calls, and status changes must never bypass the DB functions.
func Transition(ctx context.Context, db *gorm.DB, objectType, objectID, to, actorJSON, reason string) error {
	return TransitionMeta(ctx, db, objectType, objectID, to, actorJSON, reason, nil)
}

// TransitionMeta is Transition with event metadata JSONB (e.g. the watcher's
// namespace_degraded_at marker for DEGRADED transitions).
func TransitionMeta(ctx context.Context, db *gorm.DB, objectType, objectID, to, actorJSON, reason string, meta []byte) error {
	fn := "fn_" + objectType + "_transition"
	if len(meta) == 0 {
		meta = []byte("{}")
	}
	return db.WithContext(ctx).Exec(
		`SELECT `+fn+`(?::uuid, ?, ?::jsonb, ?, ?::jsonb)`,
		objectID, to, actorJSON, reason, meta).Error
}

// ActorJSON builds the actor payload stored in events/audit.
func ActorJSON(tenantID, userID, role string) string {
	b, _ := json.Marshal(map[string]string{"tenant_id": tenantID, "user_id": userID, "role": role})
	return string(b)
}

// ErrNotFound is returned by scoped lookups when the row does not exist or
// the principal may not see it (callers map to HTTP 404 — no existence leak).
var ErrNotFound = gorm.ErrRecordNotFound

// LoadNamespaceScoped fetches a namespace enforcing tenant isolation and
// owner/admin visibility (single shared implementation — replaces per-package
// loadNamespace/loadOwned duplicates).
func LoadNamespaceScoped(ctx context.Context, db *gorm.DB, tenantID, userID string, isAdmin bool, id string) (Namespace, error) {
	var ns Namespace
	q := db.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND status <> ?", id, tenantID, "DELETED")
	if !isAdmin {
		q = q.Where("owner_user_id = ?", userID)
	}
	if err := q.First(&ns).Error; err != nil {
		return Namespace{}, ErrNotFound
	}
	return ns, nil
}

// JSONBytes marshals v, never fails for map/string inputs.
func JSONBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// StrPtr returns a pointer to s (audit/event nullable columns).
func StrPtr(s string) *string { return &s }
