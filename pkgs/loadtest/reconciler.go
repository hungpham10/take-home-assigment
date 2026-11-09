package loadtest

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/runner"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Reconciler polls the Runner for active load tests and advances the DB state
// machine: PROVISIONING→RUNNING, RUNNING→COMPLETED/FAILED, STOPPING→STOPPED,
// RUNNING→TIMED_OUT (timeout_at) — plus cleanup transitions.
type Reconciler struct {
	DB     *gorm.DB
	Runner runner.Runner
	// K8s is the cluster client for run-scoped NetworkPolicy + script
	// ConfigMap cleanup when a run reaches a terminal status (nil in
	// legacy/local mode).
	K8s k8s.Client
	Every  time.Duration
	Now    func() time.Time
}

// Start blocks until ctx is done; run in a goroutine.
func (rc Reconciler) Start(ctx context.Context) {
	t := time.NewTicker(rc.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rc.tick(ctx)
		}
	}
}

func (rc Reconciler) tick(ctx context.Context) {
	var active []store.LoadTest
	if err := rc.DB.WithContext(ctx).
		Where("status IN ? AND runner_ref IS NOT NULL",
			[]string{"PROVISIONING", "RUNNING", "STOPPING"}).
		Limit(100).Find(&active).Error; err != nil {
		return
	}
	for i := range active {
		rc.reconcileOne(ctx, &active[i])
	}
	rc.reapTimeouts(ctx)
}

func (rc Reconciler) reconcileOne(ctx context.Context, lt *store.LoadTest) {
	st, err := rc.Runner.Status(ctx, *lt.RunnerRef)
	if err != nil {
		slog.Warn("runner status failed", "loadtest", lt.ID, "err", err)
		return
	}
	actor := store.ActorJSON("", "system", "")
	switch {
	// Runner lost the ref (e.g. controller restart wiped its in-process
	// table): STOPPED-with-Missing is NOT a normal stop — fail the run so it
	// doesn't poll a dead ref forever.
	case st.Missing && (lt.Status == "PROVISIONING" || lt.Status == "RUNNING"):
		_ = store.LTToFailed(ctx, rc.DB, lt.ID,
			actor, "runner lost the run (controller restart?): "+st.Msg)
	case lt.Status == "PROVISIONING" && st.State == "RUNNING":
		// Transition FIRST (fn_*_transition updates status+version), then
		// ancillary fields — never UPDATE status directly.
		if err := store.LTToRunning(ctx, rc.DB, lt.ID, actor, "runner started"); err == nil {
			now := rc.now()
			_ = rc.DB.Model(&store.LoadTest{}).Where("id = ?", lt.ID).
				Update("started_at", &now).Error
		}
	case lt.Status == "PROVISIONING" && (st.State == "FAILED" || st.State == "STOPPED"):
		_ = store.LTToFailed(ctx, rc.DB, lt.ID, actor, "runner failed at provision: "+st.Msg)
	case lt.Status == "RUNNING" && st.State == "COMPLETED":
		rc.finish(ctx, lt, "COMPLETED", actor, "runner completed")
	case lt.Status == "RUNNING" && st.State == "FAILED":
		rc.finish(ctx, lt, "FAILED", actor, "runner failed: "+st.Msg)
	case lt.Status == "STOPPING" && (st.State == "STOPPED" || st.State == "FAILED" || st.State == "COMPLETED"):
		rc.finish(ctx, lt, "STOPPED", actor, "stopped by request")
	}
}

func (rc Reconciler) finish(ctx context.Context, lt *store.LoadTest, to, actor, reason string) {
	err := rc.DB.Transaction(func(tx *gorm.DB) error {
		if err := store.LTFinish(ctx, tx, lt.ID, to, actor, reason); err != nil {
			return err
		}
		now := rc.now()
		return tx.Model(&store.LoadTest{}).Where("id = ?", lt.ID).
			Update("finished_at", &now).Error
	})
	if err != nil {
		slog.Warn("finish transition failed", "loadtest", lt.ID, "to", to, "err", err)
		return
	}
	// Run reached a terminal status: delete run-scoped NetworkPolicies +
	// script ConfigMap so they don't linger after the run (docs §3.3).
	if rc.K8s != nil {
		envNS := envreconciler.EnvNamespaceName(lt.NamespaceID)
		if err := envreconciler.DeleteRunPolicies(ctx, rc.K8s, envNS, lt.ID); err != nil {
			slog.Warn("delete run policies failed", "loadtest", lt.ID, "err", err)
		}
	}
}

// reapTimeouts: RUNNING past timeout_at → TIMED_OUT → TIMEOUT_CLEANED.
func (rc Reconciler) reapTimeouts(ctx context.Context) {
	var timedOut []store.LoadTest
	if err := rc.DB.WithContext(ctx).
		Where("status = ? AND timeout_at IS NOT NULL AND timeout_at < ?", "RUNNING", rc.now()).
		Limit(50).Find(&timedOut).Error; err != nil {
		return
	}
	actor := store.ActorJSON("", "system", "")
	for i := range timedOut {
		lt := &timedOut[i]
		if lt.RunnerRef != nil {
			_ = rc.Runner.Stop(ctx, *lt.RunnerRef)
		}
		if err := store.LTToTimedOut(ctx, rc.DB, lt.ID, actor, "timeout exceeded"); err != nil {
			continue
		}
		_ = store.LTToTimeoutCleaned(ctx, rc.DB, lt.ID, actor, "cleaned after timeout")
		// Run reached a terminal status: delete run-scoped NetworkPolicies +
		// script ConfigMap (docs §3.3).
		if rc.K8s != nil {
			envNS := envreconciler.EnvNamespaceName(lt.NamespaceID)
			if err := envreconciler.DeleteRunPolicies(ctx, rc.K8s, envNS, lt.ID); err != nil {
				slog.Warn("delete run policies failed", "loadtest", lt.ID, "err", err)
			}
		}
	}
}

func (rc Reconciler) now() time.Time {
	if rc.Now != nil {
		return rc.Now()
	}
	return time.Now()
}
