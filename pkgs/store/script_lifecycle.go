// ---
// Single source of truth cho script lifecycle (plans/26). Enforcement:
// scripts/lint-transitions.sh.
//
//	PENDING_VALIDATION → VALIDATING            script worker
//	VALIDATING → VALIDATED                     script worker (k6 pass)
//	VALIDATING → INVALID                       script worker (k6 fail)
//	VALIDATING → VALIDATION_FAILED             script worker (runner crash)
//	INVALID | VALIDATION_FAILED → VALIDATING   handlers revalidate
//	VALIDATED → VALIDATING                     handlers revalidate (0009)

package store

import (
	"context"

	"gorm.io/gorm"
)

// ScriptStatus — CHECK constraint sql/0004 (+0009).
const (
	ScriptStatusPendingValidation = "PENDING_VALIDATION"
	ScriptStatusValidating        = "VALIDATING"
	ScriptStatusValidated         = "VALIDATED"
	ScriptStatusInvalid           = "INVALID"
	ScriptStatusValidationFailed  = "VALIDATION_FAILED"
)

func scrT(ctx context.Context, db *gorm.DB, id, to, actor, reason string) error {
	return Transition(ctx, db, "script", id, to, actor, reason)
}

// SCRToValidating chuyển sang VALIDATING (worker/handlers revalidate).
func SCRToValidating(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return scrT(ctx, db, id, ScriptStatusValidating, actor, reason)
}

// SCRToValidated chuyển sang VALIDATED (k6 pass).
func SCRToValidated(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return scrT(ctx, db, id, ScriptStatusValidated, actor, reason)
}

// SCRToInvalid chuyển sang INVALID (k6 fail).
func SCRToInvalid(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return scrT(ctx, db, id, ScriptStatusInvalid, actor, reason)
}

// SCRToValidationFailed chuyển sang VALIDATION_FAILED (runner crash).
func SCRToValidationFailed(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return scrT(ctx, db, id, ScriptStatusValidationFailed, actor, reason)
}
