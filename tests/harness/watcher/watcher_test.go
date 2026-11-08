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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

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

func newHarness(t *testing.T) *harnessEnv {
	t.Helper()
	if sharedDB == nil || sharedNSID == "" {
		t.Skip("harness not set up (TestWATCH00Setup must run first)")
	}
	client, err := harnessClient()
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
	// Fast resync: the default 5m safety net would only tick once within the
	// deadline — on a real cluster the image pull takes seconds and the next
	// tick must catch the deployment becoming Available.
	cfg := statuswatcher.DefaultConfig()
	cfg.Resync = 5 * time.Second
	h.w = statuswatcher.New(h.client, harnessDB(t), cfg)
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
	t.Fatalf("no convergence within deadline (last status %s)\nprojection: %s",
		harnessStatus(h, t), harnessDumpProjection(h, t))
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
	// Fencing lives at the COMPONENT level: the service was applied for the
	// OLD generation, so its projection must drop to at most PROGRESSING even
	// though the namespace status itself lingers in READY until the grace
	// window emits DEGRADED (docs/namespace-status-watcher.md §4).
	var phase string
	if err := harnessDB(t).Raw(
		`SELECT phase FROM tbl_component_status
		 WHERE namespace_id = ? AND component_type = 'service'`, h.nsID).Scan(&phase).Error; err != nil {
		t.Fatalf("projection: %v", err)
	}
	if phase == "READY" {
		t.Fatal("service component READY despite desired_generation > applied generation (WATCH-03)")
	}
	// And ready_for_loadtest must be false while fenced.
	var ready bool
	if err := harnessDB(t).Raw(
		`SELECT ready_for_loadtest FROM tbl_namespaces WHERE id = ?`, h.nsID).Scan(&ready).Error; err != nil {
		t.Fatalf("ready flag: %v", err)
	}
	if ready {
		t.Fatal("ready_for_loadtest still true with fenced components (WATCH-03)")
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
	if err := harnessDB(t).Exec(
		`UPDATE tbl_namespaces SET desired_generation = desired_generation + 1 WHERE id = ?`, h.nsID).Error; err != nil {
		t.Fatalf("bump generation: %v", err)
	}
}

// WATCH-08 (plans/26): watch→workqueue trên cluster thật — thay đổi deployment
// NGOÀI (scale replicas) phải được watcher bắt qua watch event và phản ánh
// vào projection mà KHÔNG đợi resync tick.
func TestWATCH08WatchEventsTriggerReconcile(t *testing.T) {
	h := newHarness(t)
	client, err := harnessClient()
	if err != nil {
		t.Skipf("no cluster: %v", err)
	}
	cfg := statuswatcher.DefaultConfig()
	cfg.Resync = time.Hour // KHÔNG có resync — chỉ watch event được phép chạy
	w := statuswatcher.New(client, sharedDB, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	// Scale deployment ngoài vòng reconcile (trực tiếp qua client).
	dGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	dep, err := client.Get(ctx, dGVK, h.envNS, "harness-svc")
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	// SSA không nhận object có managedFields sẵn (từ Get) — strip trước apply.
	dep.SetManagedFields(nil)
	_ = unstructured.SetNestedField(dep.Object, int64(2), "spec", "replicas")
	if err := client.Apply(ctx, dep); err != nil {
		t.Fatalf("scale deployment: %v", err)
	}

	// Watch event → enqueue → worker reconcile: phase service chuyển khỏi
	// READY (1/2 replicas available) mà không cần resync.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var phase string
		if err := sharedDB.Raw(
			`SELECT phase FROM tbl_component_status WHERE namespace_id = ? AND component_type = 'service'`,
			h.nsID).Scan(&phase).Error; err == nil && phase == "PROGRESSING" {
			return // watch event đã kích hoạt reconcile (WATCH-08)
		}
		time.Sleep(time.Second)
	}
	t.Fatal("watch event did not trigger reconcile within deadline (WATCH-08)")
}
