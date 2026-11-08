// orchestration.go — wiring for the two control loops (plans/21) when the
// cluster path is enabled (K8S_ENABLED=true): Env Reconciler (desired → K8s)
// and Status Watcher (observed K8s → PG). Without the flag nothing here runs
// and the legacy synchronous apply path serves the MVP stack.
package main

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/mockcontrol"
	"github.com/hungpham/loadtest-platform/pkgs/statuswatcher"
)

// startOrchestration boots the two loops; both stop when ctx is cancelled.
func startOrchestration(ctx context.Context, db *gorm.DB, apiURL string, resync, grace, stable, applyDeadline, staleness time.Duration) error {
	client, err := k8s.NewDynamicClient(apiURL, "loadtest-controller")
	if err != nil {
		return err
	}
	rc := envreconciler.New(client, envreconciler.Options{})
	mockcontrol.Attach(rc) // binding ConfigMaps + MOCK_* env injection (step 5)
	go rc.Start(ctx, envreconciler.LoadEnvs(db))

	w := statuswatcher.New(client, db, statuswatcher.Config{
		Resync:             resync,
		Grace:              grace,
		Stable:             stable,
		ApplyDeadline:      applyDeadline,
		StalenessThreshold: staleness,
	})
	go w.Start(ctx)
	slog.Info("orchestration loops started", "api", apiURL, "resync", resync)
	return nil
}

// applyNowFn builds the request-path hierarchical apply hook (plans/26): after
// desired-state commit, POST /apply drives the Applier tree outside→in
// immediately (best-effort; the loop + watcher converge level-triggered).
func applyNowFn(db *gorm.DB, apiURL string) func(context.Context, string) {
	client, err := k8s.NewDynamicClient(apiURL, "loadtest-controller")
	if err != nil {
		slog.Warn("apply-now disabled: cluster client unavailable", "err", err)
		return nil
	}
	return func(ctx context.Context, namespaceID string) {
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
