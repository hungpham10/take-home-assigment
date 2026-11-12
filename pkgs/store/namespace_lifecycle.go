// ---
// Single source of truth cho namespace lifecycle (plans/26 — phản hồi review
// "trạng thái nằm rải rác"): mọi chuyển trạng thái namespace đi qua wrapper
// trong file này. Enforcement: scripts/lint-transitions.sh chặn raw
// store.Transition ngoài pkgs/store/.
//
// Bảng chuyển đổi + caller-owner (thêm trạng thái mới: sửa CHECK sql/0003 +
// seed + wrapper tại đây — một nơi review duy nhất):
//
//	VALIDATING → INVALID       namespace runValidation (gates fail)
//	VALIDATING → APPLYING      namespace runValidation (gates pass)
//	APPLYING → DATA_LOADING    legacy apply (watcher không gọi trực tiếp)
//	APPLYING → READY           watcher aggregate / legacy apply
//	APPLYING → DEGRADED        watcher aggregate (apply deadline)
//	APPLYING → INVALID         watcher aggregate (component FAILED)
//	DATA_LOADING → READY       watcher aggregate (restore xong)
//	DATA_LOADING → DEGRADED    watcher aggregate
//	DATA_LOADING → INVALID     watcher aggregate (restore failure)
//	READY → DEGRADED           watcher aggregate (con not-ready quá grace)
//	DEGRADED → READY           watcher aggregate (ổn định quá stable window)
//	DEGRADED → VALIDATING      namespace (spec sửa — revalidate)
//	READY/DEGRADED/INVALID → DELETING   namespace DELETE handler
//	DELETING → DELETED         reaper

package store

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// NamespaceStatus — các trạng thái hợp lệ (CHECK sql/0003).
const (
	NamespaceStatusDraft       = "DRAFT"
	NamespaceStatusValidating  = "VALIDATING"
	NamespaceStatusInvalid     = "INVALID"
	NamespaceStatusApplying    = "APPLYING"
	NamespaceStatusDataLoading = "DATA_LOADING"
	NamespaceStatusReady       = "READY"
	NamespaceStatusDegraded    = "DEGRADED"
	NamespaceStatusDeleting    = "DELETING"
	NamespaceStatusDeleted     = "DELETED"
)

// nsT gọi fn_namespace_transition kèm metadata.
func nsT(ctx context.Context, db *gorm.DB, id, to, actor, reason string, meta []byte) error {
	return TransitionMeta(ctx, db, "namespace", id, to, actor, reason, meta)
}

// NSToValidating chuyển sang VALIDATING (runValidation).
func NSToValidating(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return nsT(ctx, db, id, NamespaceStatusValidating, actor, reason, nil)
}

// NSToInvalid chuyển sang INVALID (gates fail).
func NSToInvalid(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return nsT(ctx, db, id, NamespaceStatusInvalid, actor, reason, nil)
}

// NSToApplying chuyển sang APPLYING (validation pass).
func NSToApplying(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return nsT(ctx, db, id, NamespaceStatusApplying, actor, reason, nil)
}

// NSToDataLoading chuyển sang DATA_LOADING (legacy apply; watcher không gọi trực tiếp).
func NSToDataLoading(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return nsT(ctx, db, id, NamespaceStatusDataLoading, actor, reason, nil)
}

// NSToReady chuyển sang READY (watcher aggregate / legacy apply).
func NSToReady(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return nsT(ctx, db, id, NamespaceStatusReady, actor, reason, nil)
}

// NSToDegraded chuyển sang DEGRADED kèm metadata namespace_degraded_at (docs §6:
// run RUNNING tiếp tục, kết quả flag unreliable).
func NSToDegraded(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return nsT(ctx, db, id, NamespaceStatusDegraded, actor, reason,
		JSONBytes(map[string]any{"namespace_degraded_at": time.Now().UTC()}))
}

// NSToDeleting chuyển sang DELETING (DELETE handler).
func NSToDeleting(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return nsT(ctx, db, id, NamespaceStatusDeleting, actor, reason, nil)
}

// NSToDeleted chuyển sang DELETED (reaper).
func NSToDeleted(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return nsT(ctx, db, id, NamespaceStatusDeleted, actor, reason, nil)
}
