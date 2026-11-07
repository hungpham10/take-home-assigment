package loadtest

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/runner"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Reconciler polls the Runner for active load tests and advances the DB state
// machine: PROVISIONING→RUNNING, RUNNING→COMPLETED/FAILED, STOPPING→STOPPED,
// RUNNING→TIMED_OUT (timeout_at) — plus cleanup transitions.
type Reconciler struct {
	DB     *gorm.DB
	Runner runner.Runner
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
	case lt.Status == "PROVISIONING" && st.State == "RUNNING":
		// Transition FIRST (fn_*_transition updates status+version), then
		// ancillary fields — never UPDATE status directly.
		if err := store.Transition(ctx, rc.DB, "loadtest", lt.ID, "RUNNING", actor, "runner started"); err == nil {
			now := rc.now()
			_ = rc.DB.Model(&store.LoadTest{}).Where("id = ?", lt.ID).
				Update("started_at", &now).Error
		}
	case lt.Status == "PROVISIONING" && (st.State == "FAILED" || st.State == "STOPPED"):
		_ = store.Transition(ctx, rc.DB, "loadtest", lt.ID, "FAILED", actor, "runner failed at provision: "+st.Msg)
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
		if err := store.Transition(ctx, tx, "loadtest", lt.ID, to, actor, reason); err != nil {
			return err
		}
		now := rc.now()
		return tx.Model(&store.LoadTest{}).Where("id = ?", lt.ID).
			Update("finished_at", &now).Error
	})
	if err != nil {
		slog.Warn("finish transition failed", "loadtest", lt.ID, "to", to, "err", err)
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
		if err := store.Transition(ctx, rc.DB, "loadtest", lt.ID, "TIMED_OUT", actor, "timeout exceeded"); err != nil {
			continue
		}
		_ = store.Transition(ctx, rc.DB, "loadtest", lt.ID, "TIMEOUT_CLEANED", actor, "cleaned after timeout")
	}
}

func (rc Reconciler) now() time.Time {
	if rc.Now != nil {
		return rc.Now()
	}
	return time.Now()
}
