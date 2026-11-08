//go:build harness_k8s

package watcher_harness

import (
	"fmt"
	"testing"
	"time"

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
