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
