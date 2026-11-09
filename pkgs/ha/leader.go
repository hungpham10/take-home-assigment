// Package ha provides high-availability primitives for the loadtest
// controller: a K8s Lease-based leader-election Gate so only one active
// pod runs the background workers (Env Reconciler, Status Watcher, run
// reconciler/reaper) at a time.
package ha

import (
	"context"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Gate wraps client-go leader election. IsLeader() is safe for concurrent use.
// Nil gate = always leader (single-pod / non-HA mode).
type Gate struct {
	leader    atomic.Bool
	elector   *leaderelection.LeaderElector
	initErr   error
}

// Config for leader election. Times match docs/k8s-orchestration.md §10.
type Config struct {
	// Identity unique per pod (e.g. pod name / hostname).
	Identity string
	// LeaseName name of the coordination Lease resource.
	LeaseName string
	// LeaseNamespace namespace holding the Lease.
	LeaseNamespace string
	// LeaseDuration how long a leader lease is valid.
	LeaseDuration time.Duration
	// RenewDeadline how long before expiry to renew.
	RenewDeadline time.Duration
	// RetryPeriod how often to try acquiring/renewing.
	RetryPeriod time.Duration
}

// DefaultConfig returns sane defaults for MVP HA.
func DefaultConfig(identity, leaseName, leaseNamespace string) Config {
	return Config{
		Identity:         identity,
		LeaseName:        leaseName,
		LeaseNamespace:   leaseNamespace,
		LeaseDuration:    15 * time.Second,
		RenewDeadline:    10 * time.Second,
		RetryPeriod:      2 * time.Second,
	}
}

// NewGate creates a Gate backed by a K8s Lease lock.
func NewGate(client kubernetes.Interface, cfg Config) *Gate {
	g := &Gate{}
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      cfg.LeaseName,
			Namespace: cfg.LeaseNamespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: cfg.Identity,
		},
	}
	leaderCallbacks := leaderelection.LeaderCallbacks{
		OnStartedLeading: func(_ context.Context) {
			g.leader.Store(true)
		},
		OnStoppedLeading: func() {
			g.leader.Store(false)
		},
	}
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: cfg.LeaseDuration,
		RenewDeadline: cfg.RenewDeadline,
		RetryPeriod:   cfg.RetryPeriod,
		Callbacks:     leaderCallbacks,
		Name:          cfg.LeaseName,
	})
	if err != nil {
		// Should not happen with valid config; fail closed — gate stays false
		// until a valid elector exists. Run() will return error immediately.
		g.elector = nil
		g.initErr = err
		return g
	}
	g.elector = elector
	return g
}

// IsLeader returns whether this instance currently holds the lease.
func (g *Gate) IsLeader() bool {
	return g.leader.Load()
}

// Run blocks running leader election until ctx is cancelled.
func (g *Gate) Run(ctx context.Context) error {
	if g.elector == nil {
		if g.initErr != nil {
			return g.initErr
		}
		return context.Canceled
	}
	g.elector.Run(ctx)
	return nil
}
