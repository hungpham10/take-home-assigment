// ---
// Single source of truth cho dataset lifecycle (plans/26). Enforcement:
// scripts/lint-transitions.sh.
//
//	IMPORTING → AVAILABLE              namespace runDatasetValidation (pass)
//	IMPORTING → INVALID                namespace (PII/checksum fail)
//	AVAILABLE → SUSPENDED              namespace suspend
//	AVAILABLE → EXPIRED                namespace (retention hết hạn)
//	AVAILABLE → DELETING               namespace DELETE
//	SUSPENDED → AVAILABLE | DELETING   namespace
//	EXPIRED → DELETING                 namespace
//	DELETING → DELETED                 namespace (cleanup xong)

package store

import (
	"context"

	"gorm.io/gorm"
)

// DatasetStatus — CHECK constraint sql/0005.
const (
	DatasetStatusImporting = "IMPORTING"
	DatasetStatusAvailable = "AVAILABLE"
	DatasetStatusInvalid   = "INVALID"
	DatasetStatusExpired   = "EXPIRED"
	DatasetStatusSuspended = "SUSPENDED"
	DatasetStatusDeleting  = "DELETING"
	DatasetStatusDeleted   = "DELETED"
)

func dsT(ctx context.Context, db *gorm.DB, id, to, actor, reason string) error {
	return Transition(ctx, db, "dataset", id, to, actor, reason)
}

// DSToAvailable chuyển sang AVAILABLE (validation pass).
func DSToAvailable(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return dsT(ctx, db, id, DatasetStatusAvailable, actor, reason)
}

// DSToInvalid chuyển sang INVALID (PII/checksum fail).
func DSToInvalid(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return dsT(ctx, db, id, DatasetStatusInvalid, actor, reason)
}

// DSToSuspended chuyển sang SUSPENDED.
func DSToSuspended(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return dsT(ctx, db, id, DatasetStatusSuspended, actor, reason)
}

// DSToExpired chuyển sang EXPIRED (retention).
func DSToExpired(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return dsT(ctx, db, id, DatasetStatusExpired, actor, reason)
}

// DSToDeleting chuyển sang DELETING (DELETE handler).
func DSToDeleting(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return dsT(ctx, db, id, DatasetStatusDeleting, actor, reason)
}

// DSToDeleted chuyển sang DELETED (cleanup xong).
func DSToDeleted(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return dsT(ctx, db, id, DatasetStatusDeleted, actor, reason)
}
