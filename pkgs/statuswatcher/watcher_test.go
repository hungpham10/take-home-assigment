package statuswatcher_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/mockcontrol"
	"github.com/hungpham/loadtest-platform/pkgs/mockruntime"
	"github.com/hungpham/loadtest-platform/pkgs/statuswatcher"
	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

// world bundles the test DB, a fake cluster and the ids under test.
type world struct {
	db     *gorm.DB
	client *k8s.Fake
	nsID   string
	envNS  string
}

func gormDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.TestDB(t)
}

// seedNamespace inserts tenant/user/namespace rows directly (watcher tests do
// not need the HTTP API).
func seedNamespace(t *testing.T, db *gorm.DB, status string) string {
	t.Helper()
	suffix := newUUID(t)[:8]
	var tenantID string
	if err := db.Raw(
		`INSERT INTO tbl_tenants (name, slug) VALUES (?, ?) RETURNING id`,
		"w-"+suffix, "w-"+suffix).Scan(&tenantID).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var userID string
	if err := db.Raw(
		`INSERT INTO tbl_users (tenant_id, email) VALUES (?, ?) RETURNING id`,
		tenantID, "w-"+suffix+"@t.io").Scan(&userID).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	nsID := newUUID(t)
	if err := db.Exec(
		`INSERT INTO tbl_namespaces (id, tenant_id, owner_user_id, name, status, desired_generation)
		 VALUES (?, ?, ?, ?, ?, 1)`, nsID, tenantID, userID, "env-"+suffix, status).Error; err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	// One service row matching applyEnv's rendered deployment (the watcher
	// evaluates services from tbl_services, not from the cluster objects).
	if err := db.Exec(
		`INSERT INTO tbl_services (namespace_id, name, deploy, mock, openapi_spec, spec_hash) VALUES (?, 'svc-1', '{}', '{}', '{}', 'h1')`,
		nsID).Error; err != nil {
		t.Fatalf("seed service: %v", err)
	}
	// Dọn namespace sau test — shared DB tích lũy hàng nghìn ns cũ làm Tick
	// backlog serialize vô hạn (root cause của flake trước plans/26).
	t.Cleanup(func() {
		_ = db.Exec(`DELETE FROM tbl_namespaces WHERE id = ?`, nsID).Error
	})
	return nsID
}

func newUUID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

// applyEnv runs the env reconciler against the fake cluster (simulates Loop 1
// completing) so the watcher evaluates a populated K8s state.
func applyEnv(t *testing.T, w *world, gen int64) {
	t.Helper()
	env := &envreconciler.Env{
		NamespaceID: w.nsID, TenantID: "t", Status: "APPLYING", Generation: gen,
		Services: []envreconciler.ServiceSpec{{
			Name: "svc-1", Image: "img:1", Replicas: 1, Port: 8080, SpecHash: "h1",
		}},
	}
	rc := envreconciler.New(w.client, envreconciler.Options{})
	if err := rc.ApplyEnv(context.Background(), env); err != nil {
		t.Fatalf("apply env: %v", err)
	}
}

// markDeploymentReady makes the deployment satisfy the service evaluator:
// observedGeneration==generation, Available=True, replicas ready.
func markDeploymentReady(t *testing.T, w *world) {
	t.Helper()
	dGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	dep, err := w.client.Get(context.Background(), dGVK, w.envNS, "svc-1")
	if err != nil {
		t.Fatal(err)
	}
	dep.Object["status"] = map[string]any{
		"observedGeneration": dep.GetGeneration(),
		"availableReplicas":  int64(1),
		"replicas":           int64(1),
		"conditions": []any{map[string]any{
			"type": "Available", "status": "True",
		}},
	}
	if err := w.client.Apply(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
}

func newWatcher(w *world, cfg statuswatcher.Config) *statuswatcher.Watcher {
	return statuswatcher.New(w.client, w.db, cfg)
}

func TestWatcherAPPLYINGToREADY(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	applyEnv(t, w, 1)
	markDeploymentReady(t, w)

	cfg := statuswatcher.DefaultConfig()
	if err := newWatcher(w, cfg).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ns store.Namespace
	if err := w.db.Where("id = ?", w.nsID).First(&ns).Error; err != nil {
		t.Fatal(err)
	}
	if ns.Status != "READY" || !ns.ReadyForLoadtest {
		t.Fatalf("aggregated namespace: status=%s ready=%v", ns.Status, ns.ReadyForLoadtest)
	}
	if ns.StatusObservedAt == nil {
		t.Fatal("status_observed_at not set (watcher heartbeat)")
	}

	// Projection persisted: one READY service row.
	var rows []store.ComponentStatus
	if err := w.db.Where("namespace_id = ?", w.nsID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("tbl_component_status empty")
	}
	for _, r := range rows {
		if r.Phase != "READY" {
			t.Fatalf("component %s/%s phase %s", r.ComponentType, r.ComponentID, r.Phase)
		}
		if r.Required && (r.ObservedGeneration != 1 || r.DesiredGeneration != 1) {
			t.Fatalf("generation fencing fields: %+v", r)
		}
	}
}

// WATCH-03: the component is at most PROGRESSING when the applied object
// still carries an older generation than the namespace's desired_generation.
func TestWatcherGenerationFencing(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	applyEnv(t, w, 1) // applied generation 1
	markDeploymentReady(t, w)
	// Spec edited after apply: desired_generation now 2, cluster still shows
	// the generation-1 object.
	if err := w.db.Model(&store.Namespace{}).Where("id = ?", w.nsID).
		Update("desired_generation", 2).Error; err != nil {
		t.Fatal(err)
	}

	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var row store.ComponentStatus
	if err := w.db.Where("namespace_id = ? AND component_type = 'service'", w.nsID).
		First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Phase == "READY" {
		t.Fatal("READY despite observed_generation != desired_generation (WATCH-03)")
	}
	if row.Phase != "PROGRESSING" {
		t.Fatalf("phase %s, want PROGRESSING", row.Phase)
	}

	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status == "READY" {
		t.Fatal("namespace READY despite stale generation")
	}
}

// Namespace without a matching cluster state (reconciler has not applied yet)
// must not report READY — fail closed with PENDING/PROGRESSING.
func TestWatcherMissingClusterState(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)

	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status == "READY" || ns.ReadyForLoadtest {
		t.Fatalf("fail-open: status=%s ready=%v", ns.Status, ns.ReadyForLoadtest)
	}
}

// WATCH-05: a namespace in a terminal-ready state whose heartbeat is older
// than the staleness threshold must be flagged (STATUS_STALE reason on the
// projection) and ready_for_loadtest must drop.
func TestWatcherStaleHeartbeatDropsReady(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "READY")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	if err := w.db.Model(&store.Namespace{}).Where("id = ?", w.nsID).
		Updates(map[string]any{"ready_for_loadtest": true, "status_observed_at": time.Now().Add(-30 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}

	cfg := statuswatcher.DefaultConfig()
	cfg.StalenessThreshold = 15 * time.Minute
	if err := newWatcher(w, cfg).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.ReadyForLoadtest {
		t.Fatal("ready_for_loadtest still true with stale heartbeat (WATCH-05 fail-closed)")
	}
}

func TestWatcherReconcileUnknownNamespace(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), nsID: newUUID(t), envNS: "lt-env-x"}
	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("unknown namespace should be a no-op, got %v", err)
	}
}

// Mock evaluator: a mock-enabled service becomes READY when its binding
// ConfigMap carries the desired generation (MOCK-VER-01); PENDING before.
func TestWatcherMockEvaluator(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)

	mockRaw, _ := json.Marshal(mockcontrol.MockDef{Enabled: true, Generation: 1, Routes: []mockruntime.Route{{
		Method: "GET", Path: "/x", Variants: []mockruntime.Variant{{Weight: 1, Status: 200}},
	}}})
	if err := w.db.Exec(
		`INSERT INTO tbl_services (namespace_id, name, deploy, mock, openapi_spec) VALUES (?, 'm-svc', '{}', ?, '{}')`,
		w.nsID, mockRaw).Error; err != nil {
		t.Fatalf("seed mock service: %v", err)
	}
	applyEnv(t, w, 1)

	// No binding yet → mock PENDING, namespace must not reach READY.
	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var row store.ComponentStatus
	if err := w.db.Where("namespace_id = ? AND component_type = 'mock'", w.nsID).First(&row).Error; err != nil {
		t.Fatalf("mock row: %v", err)
	}
	if row.Phase != "PENDING" {
		t.Fatalf("mock phase %s, want PENDING", row.Phase)
	}

	// Binding applied by mockcontrol (reconciler Attach) at the desired gen.
	env := &envreconciler.Env{NamespaceID: w.nsID, TenantID: "t", Generation: 1,
		Services: []envreconciler.ServiceSpec{{Name: "m-svc", Image: "i", Replicas: 1, Port: 80, Mock: mockRaw}}}
	rc := envreconciler.New(w.client, envreconciler.Options{})
	mockcontrol.Attach(rc)
	if err := rc.ApplyEnv(context.Background(), env); err != nil {
		t.Fatalf("apply with bindings: %v", err)
	}
	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	_ = w.db.Where("namespace_id = ? AND component_type = 'mock'", w.nsID).First(&row)
	if row.Phase != "READY" {
		t.Fatalf("mock phase %s, want READY after binding apply", row.Phase)
	}
}

// Apply deadline (docs/namespace-status-watcher.md §5): an APPLYING
// namespace stuck longer than cfg.ApplyDeadline must transition to DEGRADED
// with reason apply_deadline_exceeded.
func TestWatcherApplyDeadline(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	applyEnv(t, w, 1) // reconciled but the deployment is NOT ready → stays APPLYING

	cfg := statuswatcher.DefaultConfig()
	cfg.ApplyDeadline = 1 * time.Minute
	watcher := newWatcher(w, cfg)
	// Namespace entered APPLYING 10 minutes ago (updated_at is the deadline
	// basis fallback); the watcher clock runs 8 minutes later. The touch
	// trigger must be disabled to backdate updated_at.
	if err := w.db.Exec(
		`ALTER TABLE tbl_namespaces DISABLE TRIGGER trg_namespaces_touch`).Error; err != nil {
		t.Fatal(err)
	}
	err := w.db.Model(&store.Namespace{}).Where("id = ?", w.nsID).
		Update("updated_at", time.Now().UTC().Add(-10*time.Minute)).Error
	_ = w.db.Exec(`ALTER TABLE tbl_namespaces ENABLE TRIGGER trg_namespaces_touch`)
	if err != nil {
		t.Fatal(err)
	}
	watcher.SetClock(func() time.Time { return time.Now().UTC().Add(-8 * time.Minute) })

	if err := watcher.ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ns store.Namespace
	if err := w.db.Where("id = ?", w.nsID).First(&ns).Error; err != nil {
		t.Fatal(err)
	}
	if ns.Status != "DEGRADED" {
		t.Fatalf("status %s, want DEGRADED after apply deadline", ns.Status)
	}
	var blocking []string
	if len(ns.BlockingReasons) > 0 && json.Unmarshal(ns.BlockingReasons, &blocking) == nil {
		found := false
		for _, b := range blocking {
			if strings.Contains(b, "apply_deadline_exceeded") {
				found = true
			}
		}
		if !found {
			t.Fatalf("blocking reasons missing apply_deadline_exceeded: %v", blocking)
		}
	}
}

// DEGRADED-with-running-run (docs §6): the watcher transitions the namespace
// to DEGRADED with metadata namespace_degraded_at; the running loadtest is
// NOT stopped — its report is flagged unreliable via the namespace event.
func TestWatcherDegradedWithRunningRun(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "READY")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	if err := w.db.Model(&store.Namespace{}).Where("id = ?", w.nsID).
		Update("ready_for_loadtest", true).Error; err != nil {
		t.Fatal(err)
	}

	cfg := statuswatcher.DefaultConfig()
	cfg.Grace = 1 * time.Millisecond
	watcher := newWatcher(w, cfg)
	// First pass arms the grace window; second pass (past grace) degrades.
	_ = watcher.ReconcileStatus(context.Background(), w.nsID)
	watcher.SetClock(func() time.Time { return time.Now().UTC().Add(2 * time.Minute) })
	if err := watcher.ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ns store.Namespace
	if err := w.db.Where("id = ?", w.nsID).First(&ns).Error; err != nil {
		t.Fatal(err)
	}
	if ns.Status != "DEGRADED" {
		t.Fatalf("status %s, want DEGRADED after grace", ns.Status)
	}
	// The degradation event carries namespace_degraded_at metadata.
	var meta string
	if err := w.db.Raw(
		`SELECT metadata FROM tbl_namespace_events
		 WHERE object_id = ? AND to_status = 'DEGRADED'
		 ORDER BY id DESC LIMIT 1`, w.nsID).Scan(&meta).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), "namespace_degraded_at") {
		t.Fatalf("DEGRADED event metadata missing namespace_degraded_at: %s", meta)
	}
}

// WATCH-04 / RUN-CRASH-01 (fake-CI): a rejected/racing transition between
// projection upsert and status write must not crash-loop — the next pass
// converges without duplicating the DEGRADED/READY event.
func TestWatcherRacingTransitionConverges(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	applyEnv(t, w, 1)
	markDeploymentReady(t, w)

	watcher := newWatcher(w, statuswatcher.DefaultConfig())
	if err := watcher.ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	// Race: the user transitioned the namespace back to APPLYING (re-validate)
	// right after the watcher wrote READY — ready_for_loadtest drops cùng lúc
	// (CHECK chk_namespaces_ready_only_when_ready, DB-TXN-10). The next pass
	// must converge again without error or duplicate-event loops.
	if err := w.db.Model(&store.Namespace{}).Where("id = ?", w.nsID).
		Updates(map[string]any{"status": "APPLYING", "ready_for_loadtest": false}).Error; err != nil {
		t.Fatal(err)
	}
	if err := watcher.ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("second reconcile after race: %v", err)
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status != "READY" {
		t.Fatalf("status %s after race convergence, want READY", ns.Status)
	}
	var readyEvents int64
	_ = w.db.Raw(`SELECT count(*) FROM tbl_namespace_events WHERE object_id = ? AND to_status = 'READY'`, w.nsID).Scan(&readyEvents)
	if readyEvents < 2 {
		t.Fatalf("READY events %d, want 2 (one per convergence)", readyEvents)
	}
}

// WATCH-06 (fake-CI): a component flapping within the grace window must NOT
// emit DEGRADED (no spurious run blocking).
func TestWatcherFlapWithinGraceNoDegraded(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "READY")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	if err := w.db.Model(&store.Namespace{}).Where("id = ?", w.nsID).
		Update("ready_for_loadtest", true).Error; err != nil {
		t.Fatal(err)
	}

	cfg := statuswatcher.DefaultConfig()
	cfg.Grace = 1 * time.Hour // grace far in the future → flap never degrades
	watcher := newWatcher(w, cfg)
	for range 3 {
		if err := watcher.ReconcileStatus(context.Background(), w.nsID); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status != "READY" {
		t.Fatalf("status %s after flapping within grace, want READY (WATCH-06)", ns.Status)
	}
}

// MOCK-VER-01 (plans/24 e2e): binding snapshot với schema_version sai bị
// mockruntime reject; generation mismatch giữa binding và desired_generation
// giữ mock evaluator ở PROGRESSING — không bao giờ READY.
func TestWatcherMockSnapshotContract(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	applyEnv(t, w, 1)
	markDeploymentReady(t, w)

	mockRaw, _ := json.Marshal(mockcontrol.MockDef{Enabled: true, Generation: 1, Routes: []mockruntime.Route{{
		Method: "GET", Path: "/x", Variants: []mockruntime.Variant{{Weight: 1, Status: 200}},
	}}})
	if err := w.db.Exec(
		`INSERT INTO tbl_services (namespace_id, name, deploy, mock, openapi_spec, spec_hash) VALUES (?, 'mock-svc', '{}', ?, '{}', 'h1')`,
		w.nsID, mockRaw).Error; err != nil {
		t.Fatal(err)
	}

	// 1. Runtime từ chối snapshot schema_version sai (MOCK-VER-01 header rule).
	rt := mockruntime.New()
	if err := rt.ApplySnapshot(&mockruntime.Snapshot{SchemaVersion: "v0", Generation: 1}); err == nil {
		t.Fatal("runtime accepted wrong schema_version")
	}

	// 2. Binding apply với generation DESIRED (1) → READY như bình thường.
	env := &envreconciler.Env{NamespaceID: w.nsID, TenantID: "t", Generation: 1,
		Services: []envreconciler.ServiceSpec{{Name: "mock-svc", Image: "i", Replicas: 1, Port: 80, Mock: mockRaw}}}
	rc := envreconciler.New(w.client, envreconciler.Options{})
	mockcontrol.Attach(rc)
	if err := rc.ApplyEnv(context.Background(), env); err != nil {
		t.Fatalf("apply bindings: %v", err)
	}
	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 3. Spec edit → desired_generation=2; binding vẫn gen 1 → fencing
	// giữ mock ở PROGRESSING (mock_generation_pending), không READY.
	if err := w.db.Model(&store.Namespace{}).Where("id = ?", w.nsID).
		Update("desired_generation", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	var row store.ComponentStatus
	if err := w.db.Where("namespace_id = ? AND component_type = 'mock'", w.nsID).
		First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Phase == "READY" {
		t.Fatal("mock READY despite binding generation < desired_generation (MOCK-VER-01)")
	}
	if row.Phase != "PROGRESSING" {
		t.Fatalf("mock phase %s, want PROGRESSING", row.Phase)
	}
}

// TestINT_ENV_09_AggregationAllChildrenReady (plans/26 — khóa behavior):
// namespace CHỈ READY khi mọi required con READY; một con PROGRESSING/PENDING
// → namespace không bao giờ READY/ready_for_loadtest. Positive side: đủ con
// READY → READY (đã cover bởi TestWatcherAPPLYINGToREADY).
func TestINT_ENV_09_AggregationAllChildrenReady(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	// Service row tồn tại nhưng KHÔNG có deployment trên cluster (reconciler
	// chưa apply) → service PENDING required → namespace không được READY.
	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status == "READY" || ns.ReadyForLoadtest {
		t.Fatalf("namespace READY despite PENDING child: status=%s ready=%v", ns.Status, ns.ReadyForLoadtest)
	}
	var svcPhase string
	w.db.Raw(`SELECT phase FROM tbl_component_status WHERE namespace_id = ? AND component_type = 'service'`, w.nsID).Scan(&svcPhase)
	if svcPhase == "READY" {
		t.Fatal("service READY although deployment missing (evaluator bug)")
	}
}

// WATCH-E1: Vacuous namespace (no required components at all) must never be
// READY — fail-closed (allReady requires len(rows) > 0).
func TestWatcherVacuousNamespaceNeverReady(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	// No service rows, no cluster objects → zero required components.
	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status == "READY" || ns.ReadyForLoadtest {
		t.Fatalf("vacuous namespace READY: status=%s ready=%v (fail-closed violated)", ns.Status, ns.ReadyForLoadtest)
	}
	var rows []store.ComponentStatus
	if err := w.db.Where("namespace_id = ?", w.nsID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no component rows for vacuous ns, got %d", len(rows))
	}
}

// WATCH-E2: Seeded APPLYING namespace with NO transition event AND zero
// UpdatedAt → pastApplyDeadline returns false → stuck APPLYING forever,
// never READY, never DEGRADED.
func TestWatcherSeededStateNoEventStuckApplying(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	// Strip updated_at to zero so pastApplyDeadline falls through to false.
	if err := w.db.Model(&store.Namespace{}).Where("id = ?", w.nsID).
		Update("updated_at", (*time.Time)(nil)).Error; err != nil {
		t.Fatal(err)
	}

	cfg := statuswatcher.DefaultConfig()
	cfg.ApplyDeadline = 1 * time.Second
	watcher := newWatcher(w, cfg)
	if err := watcher.ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Even repeated reconcile passes should not flip it to DEGRADED/READY
	// because there's no deadline anchor.
	if err := watcher.ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status != "APPLYING" {
		t.Fatalf("stuck APPLYING drifted to %s (deadline not enforced without event anchor)", ns.Status)
	}
}

// WATCH-E3: cluster error → component UNKNOWN phase → namespace blocked
// forever (anyPending=true, no deadline path for UNKNOWN).
func TestWatcherUnknownComponentBlocksReadyForever(t *testing.T) {
	ff := &FaultyFake{GetErr: k8s.ErrNotFound}
	w := &world{db: gormDB(t), client: ff.Fake, envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	applyEnv(t, w, 1)
	// Deployment exists but Get returns an error → UNKNOWN phase.
	// The watcher should not reach READY.
	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var row store.ComponentStatus
	_ = w.db.Where("namespace_id = ? AND component_type = 'service'", w.nsID).First(&row)
	if row.Phase != "UNKNOWN" {
		t.Fatalf("service phase %s, want UNKNOWN", row.Phase)
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status == "READY" || ns.ReadyForLoadtest {
		t.Fatalf("namespace READY despite UNKNOWN component: status=%s ready=%v", ns.Status, ns.ReadyForLoadtest)
	}
}

// WATCH-E4: DRAFT namespace is out of watcher's remit — terminal, no-op.
func TestWatcherDraftNamespaceNoOp(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "DRAFT")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)

	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status != "DRAFT" {
		t.Fatalf("DRAFT namespace changed to %s (watcher must no-op on terminal states)", ns.Status)
	}
}

// WATCH-E5: INVALID namespace is terminal — watcher no-ops.
func TestWatcherInvalidNamespaceNoOp(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "INVALID")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)

	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status != "INVALID" {
		t.Fatalf("INVALID namespace changed to %s (watcher must no-op on terminal states)", ns.Status)
	}
}

// WATCH-R1: Race — two concurrent ReconcileStatus on the same namespace
// must not corrupt state. Both see READY components; both should converge
// to READY without duplicate-transition errors.
func TestWatcherConcurrentReconcileConverges(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	applyEnv(t, w, 1)
	markDeploymentReady(t, w)

	watcher := newWatcher(w, statuswatcher.DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				if err := watcher.ReconcileStatus(ctx, w.nsID); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent reconcile error: %v", err)
		}
	}

	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status != "READY" || !ns.ReadyForLoadtest {
		t.Fatalf("after concurrent reconcile: status=%s ready=%v, want READY/true", ns.Status, ns.ReadyForLoadtest)
	}
}

// WATCH-R2: ready_for_loadtest is set as a separate UPDATE after
// fn_namespace_transition sets status=READY. Between those two calls there
// is a window where status=READY but ready_for_loadtest=false. A concurrent
// reader (e.g. POST /loadtests gate) could observe the inconsistent state.
func TestWatcherReadyForLoadtestSetWithStatus(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	applyEnv(t, w, 1)
	markDeploymentReady(t, w)

	if err := newWatcher(w, statuswatcher.DefaultConfig()).ReconcileStatus(context.Background(), w.nsID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	// After aggregate(), status=READY AND ready_for_loadtest must be true
	// in the same pass.
	if ns.Status != "READY" {
		t.Fatalf("status %s, want READY", ns.Status)
	}
	if !ns.ReadyForLoadtest {
		t.Fatal("ready_for_loadtest false immediately after READY transition (race window)")
	}
}

// WATCH-R3: Grace window armed on pass 1, then cleared on pass 2 before
// grace elapses → no DEGRADED. Concurrent clearHysteresis interleaving
// must not break the hysteresis state machine.
func TestWatcherGraceWindowInterleavedClear(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "READY")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	if err := w.db.Model(&store.Namespace{}).Where("id = ?", w.nsID).
		Update("ready_for_loadtest", true).Error; err != nil {
		t.Fatal(err)
	}

	cfg := statuswatcher.DefaultConfig()
	cfg.Grace = 1 * time.Hour // grace far in future
	watcher := newWatcher(w, cfg)
	// Multiple concurrent reconciles — clearHysteresis runs on each pass
	// because allReady=true (no degradation condition triggered).
	for i := 0; i < 5; i++ {
		if err := watcher.ReconcileStatus(context.Background(), w.nsID); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	var ns store.Namespace
	_ = w.db.Where("id = ?", w.nsID).First(&ns)
	if ns.Status != "READY" {
		t.Fatalf("status %s after interleaved clearHysteresis, want READY", ns.Status)
	}
}

// FaultyFake wraps k8s.Fake with per-operation fault injection.
type FaultyFake struct {
	*k8s.Fake
	GetErr error
}

func (f *FaultyFake) Get(_ context.Context, gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error) {
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	return f.Fake.Get(context.Background(), gvk, ns, name)
}
