//go:build harness_k8s

package watcher_harness

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type harnessDBType = *gorm.DB

// openHarnessDB connects the harness to the control-plane Postgres (the same
// DB the controller writes — the watcher projection must be observed there).
func openHarnessDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open harness db: %v", err)
	}
	// No per-test cleanup: the handle is shared across the harness run
	// (TestWATCH00Setup opens it once); closing it would break later tests.
	return db
}

// queryHarness runs one scalar query against the harness DB.
func queryHarness(h *harnessEnv, t *testing.T, q string) string {
	t.Helper()
	var s string
	if err := harnessDB(t).Raw(q, h.nsID).Scan(&s).Error; err != nil {
		t.Fatalf("query %s: %v", q, err)
	}
	return s
}

// TestMain tears the harness environment down once ALL tests finished
// (t.Cleanup on TestWATCH00Setup would delete it right after setup — every
// later test then saw PENDING not_applied; caught on kind in CI).
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedNSID != "" {
		if client, err := harnessClient(); err == nil {
			_ = envreconciler.New(client, envreconciler.Options{}).DeleteEnv(context.Background(), sharedNSID)
		}
	}
	os.Exit(code)
}

// seedHarnessNamespace inserts tenant/user/namespace/service rows the
// watcher evaluates against the real cluster.
func seedHarnessNamespace(t *testing.T, db *gorm.DB) string {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	var tenantID string
	if err := db.Raw(
		`INSERT INTO tbl_tenants (name, slug) VALUES (?, ?) RETURNING id`,
		"h-"+suffix, "h-"+suffix).Scan(&tenantID).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var userID string
	if err := db.Raw(
		`INSERT INTO tbl_users (tenant_id, email) VALUES (?, ?) RETURNING id`,
		tenantID, "h-"+suffix+"@harness.local").Scan(&userID).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var nsID string
	if err := db.Raw(
		`INSERT INTO tbl_namespaces (id, tenant_id, owner_user_id, name, status, desired_generation)
		 VALUES (gen_random_uuid(), ?, ?, ?, 'APPLYING', 1) RETURNING id`,
		tenantID, userID, "harness-env-"+suffix).Scan(&nsID).Error; err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	if err := db.Exec(
		`INSERT INTO tbl_services (namespace_id, name, deploy, mock, openapi_spec, spec_hash)
		 VALUES (?, 'harness-svc', '{"image":"nginx:alpine","replicas":1,"port":80}', '{}', '{}', 'harness-1')`,
		nsID).Error; err != nil {
		t.Fatalf("seed service: %v", err)
	}
	return nsID
}

// harnessDumpProjection renders the current projection rows for failure
// diagnostics (phase/reason/generations per component).
func harnessDumpProjection(h *harnessEnv, t *testing.T) string {
	t.Helper()
	var rows []struct {
		ComponentType string
		ComponentID   string
		Phase         string
		Reason        *string
		Required      bool
		DesiredGen    int64 `gorm:"column:desired_generation"`
		ObservedGen   int64 `gorm:"column:observed_generation"`
		ObservedAt    time.Time
	}
	if err := harnessDB(t).Raw(
		`SELECT component_type, component_id, phase, reason, required,
		        desired_generation, observed_generation, observed_at
		 FROM tbl_component_status WHERE namespace_id = ? ORDER BY component_type`, h.nsID).Scan(&rows).Error; err != nil {
		return fmt.Sprintf("dump error: %v", err)
	}
	out := ""
	for _, r := range rows {
		out += fmt.Sprintf("%s/%s phase=%s reason=%v req=%v des=%d obs=%d at=%s | ",
			r.ComponentType, r.ComponentID, r.Phase, r.Reason, r.Required,
			r.DesiredGen, r.ObservedGen, r.ObservedAt.Format("15:04:05"))
	}
	return out
}

// TestWATCH00Setup runs FIRST (Go executes tests in file order): it applies
// a minimal environment (namespace + service deployment) to the real cluster
// via the Env Reconciler so the WATCH-01..06 cases observe genuine objects.
// harnessClient builds a cluster client: KUBECONFIG (colima/kind carry TLS
// certs there) takes precedence, bare K8S_API_URL as fallback.
func harnessClient() (k8s.Client, error) {
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		return k8s.NewClientFromKubeconfig(kc)
	}
	return k8s.NewDynamicClient(os.Getenv("K8S_API_URL"), k8s.FieldManager)
}

func TestWATCH00Setup(t *testing.T) {
	db := harnessDB(t)
	client, err := harnessClient()
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
}
