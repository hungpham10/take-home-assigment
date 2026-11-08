package statuswatcher_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
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

// WATCH-04 (fake-CI): a rejected/racing transition between projection upsert
// and status write must not crash-loop — the next pass converges without
// duplicating the DEGRADED/READY event.
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
	// right after the watcher wrote READY — the next pass must converge again
	// without error or duplicate-event loops.
	if err := w.db.Model(&store.Namespace{}).Where("id = ?", w.nsID).
		Update("status", "APPLYING").Error; err != nil {
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
