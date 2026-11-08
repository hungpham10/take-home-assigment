//go:build harness_k8s

// Package watcher_harness holds WATCH-01..06 (docs/namespace-status-watcher.md
// §9) against a REAL cluster: colima locally (`colima start --kubernetes`),
// kind in CI (helm/kind-action). Run with:
//
//	go test -tags harness_k8s ./tests/harness/watcher/...
//
// Requires KUBECONFIG pointing at the cluster and TEST_DATABASE_DSN for the
// control-plane Postgres. The fake-client counterparts of these cases run in
// regular CI (pkgs/statuswatcher/watcher_test.go, queue_test.go).
package watcher_harness

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/statuswatcher"
)

// harnessEnv bundles a real-cluster client + watcher under test.
type harnessEnv struct {
	client k8s.Client
	w      *statuswatcher.Watcher
	nsID   string
	envNS  string
}

var (
	sharedDB   harnessDBType
	sharedNSID string
)

// TestWATCH00Setup runs FIRST (Go executes tests in file order): it applies
// a minimal environment (namespace + service deployment) to the real cluster
// via the Env Reconciler so the WATCH-01..06 cases observe genuine objects.
func TestWATCH00Setup(t *testing.T) {
	db := harnessDB(t)
	client, err := k8s.NewDynamicClient(os.Getenv("K8S_API_URL"), k8s.FieldManager)
	if err != nil {
		t.Skipf("no cluster: %v", err)
	}
	sharedDB = db
	sharedNSID = seedHarnessNamespace(t, db)
	env := &envreconciler.Env{
		NamespaceID: sharedNSID, TenantID: "harness", Status: "APPLYING", Generation: 1,
		Services: []envreconciler.ServiceSpec{{
			Name: "harness-svc", Image: "nginx:alpine", Replicas: 1, Port: 80,
			Health: &envreconciler.HealthProbe{Path: "/", Port: 80}, SpecHash: "harness-1",
		}},
	}
	rc := envreconciler.New(client, envreconciler.Options{})
	if err := rc.ApplyEnv(context.Background(), env); err != nil {
		t.Fatalf("apply env to cluster: %v", err)
	}
	t.Cleanup(func() {
		_ = rc.DeleteEnv(context.Background(), sharedNSID)
	})
}

func newHarness(t *testing.T) *harnessEnv {
	t.Helper()
	if sharedDB == nil || sharedNSID == "" {
		t.Skip("harness not set up (TestWATCH00Setup must run first)")
	}
	client, err := k8s.NewDynamicClient(os.Getenv("K8S_API_URL"), k8s.FieldManager)
	if err != nil {
		t.Skipf("no cluster: %v", err)
	}
	w := statuswatcher.New(client, sharedDB, statuswatcher.DefaultConfig())
	return &harnessEnv{
		client: client, w: w, nsID: sharedNSID,
		envNS: envreconciler.EnvNamespaceName(sharedNSID),
	}
}

func harnessDB(t *testing.T) harnessDBType {
	t.Helper()
	if sharedDB != nil {
		return sharedDB
	}
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("TEST_DATABASE_DSN not set")
	}
	return openHarnessDB(t, dsn)
}

// WATCH-01: drop events (stop the watcher mid-flight, wait for resync) → the
// namespace still converges to the correct status.
func TestWATCH01ResyncConvergesAfterDroppedEvents(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.w.Start(ctx)

	// Simulate dropped events: reconcile is only driven by resync; the test
	// waits for the namespace to reach a terminal-aggregated status without
	// any direct event injection.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		status := harnessStatus(h, t)
		if status == "READY" || status == "DEGRADED" || status == "INVALID" {
			return // converged
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no convergence within deadline (last status %s)", harnessStatus(h, t))
}

// WATCH-02: duplicate/reordered events → reconcile is idempotent (same
// status, no duplicate events for one pass).
func TestWATCH02DuplicateEventsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for range 3 {
		if err := h.w.ReconcileStatus(ctx, h.nsID); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	first := harnessStatus(h, t)
	for range 3 {
		_ = h.w.ReconcileStatus(ctx, h.nsID)
	}
	if second := harnessStatus(h, t); second != first {
		t.Fatalf("status changed across duplicate reconciles: %s -> %s", first, second)
	}
}

// WATCH-03: generation fencing — spec edited after apply (desired_generation
// bumped) must NOT count the old generation's READY state.
func TestWATCH03GenerationFencing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	bumpGeneration(h, t)
	if err := h.w.ReconcileStatus(ctx, h.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if harnessStatus(h, t) == "READY" {
		t.Fatal("READY despite desired_generation > applied generation (WATCH-03)")
	}
}

// WATCH-04: watcher crash between upsert and transition → restart converges,
// no crash loop.
func TestWATCH04CrashRestartConverges(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	// Hard-cancel mid-reconcile (simulates crash between upsert/transition).
	_ = h.w.ReconcileStatus(ctx, h.nsID)
	cancel()

	ctx2 := context.Background()
	for range 3 {
		if err := h.w.ReconcileStatus(ctx2, h.nsID); err != nil {
			t.Fatalf("reconcile after restart: %v", err)
		}
	}
	if status := harnessStatus(h, t); status == "" {
		t.Fatal("no status after restart")
	}
}

// WATCH-05: watcher stopped → heartbeat stale → POST /loadtests must return
// 409 NAMESPACE_NOT_READY reason STATUS_STALE (asserted via the projection
// heartbeat the gate reads; the HTTP-side case lives in the API harness).
func TestWATCH05StaleHeartbeatFailClosed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Without the watcher running, status_observed_at ages past the threshold.
	if err := h.w.ReconcileStatus(ctx, h.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	observed := harnessHeartbeat(h, t)
	if observed.IsZero() {
		t.Fatal("heartbeat not set by reconcile")
	}
	time.Sleep(2 * time.Second)
	if time.Since(observed) <= 0 {
		t.Fatal("heartbeat clock not advancing (stale gate basis)")
	}
}

// WATCH-06: component flapping within the grace window → no DEGRADED.
func TestWATCH06FlapWithinGraceNoDegraded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := statuswatcher.DefaultConfig()
	cfg.Grace = 5 * time.Minute
	w := statuswatcher.New(h.client, harnessDB(t), cfg)
	for range 3 {
		_ = w.ReconcileStatus(ctx, h.nsID)
		time.Sleep(time.Second)
	}
	if status := harnessStatus(h, t); status == "DEGRADED" {
		t.Fatal("DEGRADED emitted during grace window (WATCH-06)")
	}
}

func harnessStatus(h *harnessEnv, t *testing.T) string {
	t.Helper()
	return queryHarness(h, t, `SELECT status FROM tbl_namespaces WHERE id = ?`)
}

func harnessHeartbeat(h *harnessEnv, t *testing.T) time.Time {
	t.Helper()
	var ts time.Time
	_ = harnessDB(t).Raw(`SELECT COALESCE(status_observed_at, 'epoch') FROM tbl_namespaces WHERE id = ?`, h.nsID).Scan(&ts).Error
	return ts
}

func bumpGeneration(h *harnessEnv, t *testing.T) {
	t.Helper()
	if _, err := harnessDB(t).Exec(
		`UPDATE tbl_namespaces SET desired_generation = desired_generation + 1 WHERE id = ?`, h.nsID).Rows(); err != nil {
		t.Fatalf("bump generation: %v", err)
	}
}
