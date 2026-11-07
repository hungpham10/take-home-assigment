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
	fn := "fn_" + objectType + "_transition"
	return db.WithContext(ctx).Exec(
		`SELECT `+fn+`(?::uuid, ?, ?::jsonb, ?)`,
		objectID, to, actorJSON, reason).Error
}

// ActorJSON builds the actor payload stored in events/audit.
func ActorJSON(tenantID, userID, role string) string {
	b, _ := json.Marshal(map[string]string{"tenant_id": tenantID, "user_id": userID, "role": role})
	return string(b)
}
