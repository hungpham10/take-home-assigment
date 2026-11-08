// ---
// Single source of truth cho loadtest lifecycle (plans/26). Enforcement:
// scripts/lint-transitions.sh — raw store.Transition ngoài pkgs/store/ bị chặn.
//
// Bảng chuyển đổi + caller-owner:
//
//	QUEUED → PROVISIONING      handlers (provision thành công)
//	QUEUED → FAILED            handlers (provision lỗi)
//	QUEUED → STOPPING          handlers (stop khi QUEUED)
//	PROVISIONING → RUNNING     reconciler (runner started)
//	PROVISIONING → FAILED      handlers/reconciler (provision thất bại)
//	PROVISIONING → STOPPING    handlers (stop)
//	RUNNING → COMPLETED        reconciler finish (runner COMPLETED)
//	RUNNING → FAILED           reconciler finish (runner FAILED)
//	RUNNING → STOPPING         handlers (stop)
//	RUNNING → TIMED_OUT        reconciler reaper
//	STOPPING → STOPPED         reconciler finish (runner STOPPED/FAILED)
//	STOPPING → CLEANUP_FAILED  reconciler finish (cleanup lỗi)
//	TIMED_OUT → TIMEOUT_CLEANED reconciler reaper (cleanup xong)
//	TIMED_OUT → CLEANUP_FAILED reconciler (cleanup sau timeout lỗi)
//	CLEANUP_FAILED → TIMEOUT_CLEANED | STOPPED  reconciler retry

package store

import (
	"context"

	"gorm.io/gorm"
)

// LoadtestStatus — CHECK constraint sql/0004.
const (
	LoadtestStatusQueued         = "QUEUED"
	LoadtestStatusProvisioning   = "PROVISIONING"
	LoadtestStatusRunning        = "RUNNING"
	LoadtestStatusCompleted      = "COMPLETED"
	LoadtestStatusFailed         = "FAILED"
	LoadtestStatusStopping       = "STOPPING"
	LoadtestStatusStopped        = "STOPPED"
	LoadtestStatusTimedOut       = "TIMED_OUT"
	LoadtestStatusTimeoutCleaned = "TIMEOUT_CLEANED"
	LoadtestStatusCleanupFailed  = "CLEANUP_FAILED"
)

func ltT(ctx context.Context, db *gorm.DB, id, to, actor, reason string) error {
	return Transition(ctx, db, "loadtest", id, to, actor, reason)
}

// LTToProvisioning chuyển sang PROVISIONING (handlers provision).
func LTToProvisioning(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return ltT(ctx, db, id, LoadtestStatusProvisioning, actor, reason)
}

// LTToRunning chuyển sang RUNNING (reconciler).
func LTToRunning(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return ltT(ctx, db, id, LoadtestStatusRunning, actor, reason)
}

// LTToCompleted chuyển sang COMPLETED (reconciler finish).
func LTToCompleted(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return ltT(ctx, db, id, LoadtestStatusCompleted, actor, reason)
}

// LTToFailed chuyển sang FAILED (provision/start/runner failure).
func LTToFailed(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return ltT(ctx, db, id, LoadtestStatusFailed, actor, reason)
}

// LTToStopping chuyển sang STOPPING (handlers stop).
func LTToStopping(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return ltT(ctx, db, id, LoadtestStatusStopping, actor, reason)
}

// LTToStopped chuyển sang STOPPED (reconciler finish).
func LTToStopped(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return ltT(ctx, db, id, LoadtestStatusStopped, actor, reason)
}

// LTToTimedOut chuyển sang TIMED_OUT (reconciler reaper).
func LTToTimedOut(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return ltT(ctx, db, id, LoadtestStatusTimedOut, actor, reason)
}

// LTToTimeoutCleaned chuyển sang TIMEOUT_CLEANED (reaper cleanup xong).
func LTToTimeoutCleaned(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return ltT(ctx, db, id, LoadtestStatusTimeoutCleaned, actor, reason)
}

// LTToCleanupFailed chuyển sang CLEANUP_FAILED (cleanup lỗi).
func LTToCleanupFailed(ctx context.Context, db *gorm.DB, id, actor, reason string) error {
	return ltT(ctx, db, id, LoadtestStatusCleanupFailed, actor, reason)
}

// LTFinish dispatch trạng thái terminal cho reconciler finish() — giữ raw
// Transition khỏi nằm ngoài pkgs/store (lint-transitions).
func LTFinish(ctx context.Context, db *gorm.DB, id, to, actor, reason string) error {
	switch to {
	case LoadtestStatusFailed:
		return LTToFailed(ctx, db, id, actor, reason)
	case LoadtestStatusStopped:
		return LTToStopped(ctx, db, id, actor, reason)
	case LoadtestStatusCompleted:
		return LTToCompleted(ctx, db, id, actor, reason)
	default:
		return Transition(ctx, db, "loadtest", id, to, actor, reason)
	}
}
