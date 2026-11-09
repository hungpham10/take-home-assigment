// orchestration.go — wiring for the two control loops (plans/21) when the
// cluster path is enabled (K8S_ENABLED=true): Env Reconciler (desired → K8s)
// and Status Watcher (observed K8s → PG). Without the flag nothing here runs
// and the legacy synchronous apply path serves the MVP stack.
//
// HA (docs/k8s-orchestration.md §10): when HAEnabled, a K8s Lease leader
// election runs alongside the loops. Only the leader writes — the reconciler
// applies to K8s, the watcher persists to PG, and ApplyNow applies on the
// request path. Standbys are no-ops (ApplyNow returns 202, leader applies
// on the next tick via level-triggered convergence).
package main

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/config"
	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/mockcontrol"
	"github.com/hungpham/loadtest-platform/pkgs/statuswatcher"
)

// startOrchestration boots the two loops; both stop when ctx is cancelled.
// isLeader gates all writes when HA is enabled (nil = always leader).
func startOrchestration(ctx context.Context, db *gorm.DB, cfg config.Config, apiURL string, isLeader func() bool, resync, grace, stable, applyDeadline, staleness time.Duration) error {
	client, err := k8s.NewClientFromEnv(apiURL)
	if err != nil {
		return err
	}

	rc := envreconciler.New(client, envreconciler.Options{
		IsLeader:         isLeader,
		GenerationLoader: envreconciler.LoadGeneration(db),
	})
	mockcontrol.Attach(rc) // binding ConfigMaps + MOCK_* env injection (step 5)
	go rc.Start(ctx, envreconciler.LoadEnvs(db))

	w := statuswatcher.New(client, db, statuswatcher.Config{
		Resync:             resync,
		Grace:              grace,
		Stable:             stable,
		ApplyDeadline:      applyDeadline,
		StalenessThreshold: staleness,
		IsLeader:           isLeader,
	})
	go w.Start(ctx)
	slog.Info("orchestration loops started", "api", apiURL, "resync", resync, "ha", cfg.HAEnabled)
	return nil
}

// applyNowFn builds the request-path hierarchical apply hook (plans/26): after
// desired-state commit, POST /apply drives the Applier tree outside→in
// immediately (best-effort; the loop + watcher converge level-triggered).
//
// HA: only the leader applies. A standby returns no-op — the handler already
// committed desired_generation++ and will answer 202; the leader applies on
// the next tick (level-triggered convergence).
func applyNowFn(db *gorm.DB, apiURL string, isLeader func() bool) func(context.Context, string) {
	client, err := k8s.NewClientFromEnv(apiURL)
	if err != nil {
		slog.Warn("apply-now disabled: cluster client unavailable", "err", err)
		return nil
	}
	return func(ctx context.Context, namespaceID string) {
		if isLeader != nil && !isLeader() {
			// Standby: no-op. Handler bumped desired_generation + 202;
			// leader applies via tick.
			return
		}
		envs, err := envreconciler.LoadEnvs(db)(ctx)
		if err != nil {
			slog.Warn("apply-now: load envs", "err", err)
			return
		}
		for _, env := range envs {
			if env.NamespaceID != namespaceID {
				continue
			}
			if err := envreconciler.ApplyTree(ctx, client, env); err != nil {
				slog.Warn("apply-now: tree apply (watcher sẽ retry)", "namespace", namespaceID, "err", err)
			}
			return
		}
	}
}
