package namespace_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

// derefOf dereferences a nullable string (models use pointer columns).
func derefOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// tenantIDOf extracts the tenant from principal headers.
func tenantIDOf(headers map[string]string) string { return headers["X-Tenant-Id"] }

func newUserID(t *testing.T, db *gorm.DB, tenantID string) string {
	t.Helper()
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	user := store.User{TenantID: tenantID, Email: "u-" + suffix + "@x.co", DisplayName: "U"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	// RequirePrincipalWithRole resolves the role server-side from tbl_user_roles.
	if err := db.Create(&store.UserRole{UserID: user.ID, Role: "user"}).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}
	return user.ID
}

// Tests for plan 18 PR C: databases CRUD, datasets lifecycle, ingress CRUD,
// views + mock stubs.

func dbBody(name string) map[string]any {
	return map[string]any{
		"name":           name,
		"engine":         "postgres",
		"operator":       "cloudnative-pg",
		"engine_version": "16",
		"config":         map[string]any{"storage": "10Gi"},
		"ephemeral":      map[string]any{"enabled": false},
	}
}

func TestDatabaseCRUD(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "ns-db1"), map[string]string{"name": "db-env"})
	if rec.Code != 201 {
		t.Fatalf("create ns: %d %s", rec.Code, rec.Body.String())
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)
	nsBase := base + "/" + ns.ID

	// POST /databases → 201, db- prefix, engine defaults
	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/databases", testutil.WithKey(principal, "db1"), dbBody("orders"))
	if rec.Code != 201 {
		t.Fatalf("create database: %d %s", rec.Code, rec.Body.String())
	}
	var dbr store.Database
	_ = json.Unmarshal(rec.Body.Bytes(), &dbr)
	if dbr.DatabaseID == nil || !strings.HasPrefix(*dbr.DatabaseID, "db-") {
		t.Fatalf("database_id = %v, want db-*", dbr.DatabaseID)
	}
	if dbr.Engine != "postgres" || dbr.Operator != "cloudnative-pg" || derefOf(dbr.EngineVersion) != "16" {
		t.Errorf("database row = %+v", dbr)
	}

	// client-supplied database_id → 422
	bad := dbBody("other")
	bad["database_id"] = "db-custom"
	if rec = testutil.Do(t, h, http.MethodPost, nsBase+"/databases", testutil.WithKey(principal, "db2"), bad); rec.Code != 422 {
		t.Errorf("client database_id: %d, want 422", rec.Code)
	}
	// engine != postgres → 422
	bad = dbBody("other")
	bad["engine"] = "mysql"
	if rec = testutil.Do(t, h, http.MethodPost, nsBase+"/databases", testutil.WithKey(principal, "db3"), bad); rec.Code != 422 {
		t.Errorf("mysql engine: %d, want 422", rec.Code)
	}

	// GET list + detail
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/databases", principal, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "orders") {
		t.Errorf("list: %d %s", rec.Code, rec.Body.String())
	}
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/databases/"+*dbr.DatabaseID, principal, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), *dbr.DatabaseID) {
		t.Errorf("detail: %d %s", rec.Code, rec.Body.String())
	}

	// PATCH engine_version (merge — name preserved), version bumped
	rec = testutil.Do(t, h, http.MethodPatch, nsBase+"/databases/"+*dbr.DatabaseID, principal,
		map[string]any{"engine_version": "17"})
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var patched store.Database
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if derefOf(patched.EngineVersion) != "17" || patched.Name != "orders" || patched.Version != dbr.Version+1 {
		t.Errorf("patched = %+v (want engine_version 17, name kept, version bump)", patched)
	}

	// DELETE free → 204; then 404 on detail
	rec = testutil.Do(t, h, http.MethodDelete, nsBase+"/databases/"+*dbr.DatabaseID, principal, nil)
	if rec.Code != 204 {
		t.Fatalf("delete free: %d %s", rec.Code, rec.Body.String())
	}
	if rec = testutil.Do(t, h, http.MethodGet, nsBase+"/databases/"+*dbr.DatabaseID, principal, nil); rec.Code != 404 {
		t.Errorf("detail after delete: %d, want 404", rec.Code)
	}

	// 409 when a service binds the database
	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/databases", testutil.WithKey(principal, "db4"), dbBody("bound"))
	if rec.Code != 201 {
		t.Fatalf("create bound db: %d", rec.Code)
	}
	var bound store.Database
	_ = json.Unmarshal(rec.Body.Bytes(), &bound)
	svcBody := serviceFull("binder")
	svcBody["database_bindings"] = []map[string]any{{"name": "orders", "database_id": *bound.DatabaseID}}
	if rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, svcBody); rec.Code != 201 {
		t.Fatalf("create service with binding: %d %s", rec.Code, rec.Body.String())
	}
	rec = testutil.Do(t, h, http.MethodDelete, nsBase+"/databases/"+*bound.DatabaseID, principal, nil)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "CONFLICT") {
		t.Errorf("delete referenced: %d %s, want 409 CONFLICT", rec.Code, rec.Body.String())
	}
}

func TestDatasetLifecycle(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/datasets"
	principal := testutil.TenantUserHeaders(t, db, "user")

	dsBody := func(name string) map[string]any {
		return map[string]any{
			"name":               name,
			"engine":             "postgres",
			"location":           map[string]any{"provider": "s3", "bucket": "snapshots", "key": name + ".dump", "region": "ap-southeast-1"},
			"pii_classification": "masked",
			"restore_format":     "pg_dump_custom",
			"restore_policy":     map[string]any{"timeout": "30m", "parallel_jobs": 4},
		}
	}

	// POST → 202 IMPORTING→AVAILABLE (sync validation), status_url set
	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "ds1"), dsBody("orders-snap"))
	if rec.Code != 202 {
		t.Fatalf("create dataset: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		StatusURL string `json:"status_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Status != "AVAILABLE" || created.StatusURL != base+"/"+created.ID {
		t.Errorf("created = %+v", created)
	}
	var row store.Dataset
	if err := db.First(&row, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "AVAILABLE" || row.SourceRef != "s3://snapshots/orders-snap.dump" {
		t.Errorf("dataset row = %+v", row)
	}

	// bad provider / bad pii / bad engine → 422
	for field, val := range map[string]any{
		"location":           map[string]any{"provider": "gs", "bucket": "b", "key": "k"},
		"pii_classification": "secret",
		"engine":             "mysql",
	} {
		body := dsBody("bad-" + field)
		body[field] = val
		if rec = testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "ds-"+field), body); rec.Code != 422 {
			t.Errorf("%s invalid: %d, want 422", field, rec.Code)
		}
	}

	// GET list: owner sees own
	rec = testutil.Do(t, h, http.MethodGet, base, principal, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), created.ID) {
		t.Errorf("owner list: %d %s", rec.Code, rec.Body.String())
	}

	// same-tenant other user: owner-only → empty list + 404 detail
	otherUser := testutil.PrincipalHeaders(tenantIDOf(principal), newUserID(t, db, tenantIDOf(principal)), "user")
	rec = testutil.Do(t, h, http.MethodGet, base, otherUser, nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), created.ID) {
		t.Errorf("same-tenant other user list: %d %s (want empty)", rec.Code, rec.Body.String())
	}
	if rec = testutil.Do(t, h, http.MethodGet, base+"/"+created.ID, otherUser, nil); rec.Code != 404 {
		t.Errorf("same-tenant other user detail: %d, want 404", rec.Code)
	}
	// other-tenant user: empty list + 404 detail
	otherTenant := testutil.TenantUserHeaders(t, db, "user")
	rec = testutil.Do(t, h, http.MethodGet, base, otherTenant, nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), created.ID) {
		t.Errorf("other-tenant list: %d %s (want empty)", rec.Code, rec.Body.String())
	}
	if rec = testutil.Do(t, h, http.MethodGet, base+"/"+created.ID, otherTenant, nil); rec.Code != 404 {
		t.Errorf("other-tenant detail: %d, want 404", rec.Code)
	}

	// PATCH retention_until
	retention := "2027-01-01T00:00:00Z"
	rec = testutil.Do(t, h, http.MethodPatch, base+"/"+created.ID, principal,
		map[string]any{"retention_until": retention})
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var patched store.Dataset
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched.RetentionUntil == nil || patched.RetentionUntil.UTC().Format("2006-01-02T15:04:05Z") != retention {
		t.Errorf("retention_until = %v, want %s", patched.RetentionUntil, retention)
	}

	// POST validate re-runs → 202 AVAILABLE
	rec = testutil.Do(t, h, http.MethodPost, base+"/"+created.ID+"/validate", principal, nil)
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"AVAILABLE"`) {
		t.Errorf("validate: %d %s", rec.Code, rec.Body.String())
	}

	// DELETE → 409 when a namespace database references it
	rec = testutil.Do(t, h, http.MethodPost, "/api/loadtest/v1/namespaces", testutil.WithKey(principal, "ns-ds"), map[string]string{"name": "ds-env"})
	if rec.Code != 201 {
		t.Fatalf("create ns: %d", rec.Code)
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)
	rec = testutil.Do(t, h, http.MethodPost, "/api/loadtest/v1/namespaces/"+ns.ID+"/databases",
		testutil.WithKey(principal, "db-ds"), map[string]any{
			"name":        "restored",
			"data_source": map[string]any{"dataset_id": created.ID, "mode": "restore"},
		})
	if rec.Code != 201 {
		t.Fatalf("create db with data_source: %d %s", rec.Code, rec.Body.String())
	}
	rec = testutil.Do(t, h, http.MethodDelete, base+"/"+created.ID, principal, nil)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "CONFLICT") {
		t.Errorf("delete referenced: %d %s, want 409 CONFLICT", rec.Code, rec.Body.String())
	}

	// unreferenced dataset → 200 DELETED
	rec = testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "ds2"), dsBody("loose-snap"))
	if rec.Code != 202 {
		t.Fatalf("create loose: %d", rec.Code)
	}
	var loose struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &loose)
	rec = testutil.Do(t, h, http.MethodDelete, base+"/"+loose.ID, principal, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "DELETED") {
		t.Errorf("delete unreferenced: %d %s", rec.Code, rec.Body.String())
	}
}

func TestIngressCRUD(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "ns-ing"), map[string]string{"name": "ing-env"})
	if rec.Code != 201 {
		t.Fatalf("create ns: %d", rec.Code)
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)
	nsBase := base + "/" + ns.ID

	// need a service for the rule
	if rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, serviceFull("widgets")); rec.Code != 201 {
		t.Fatalf("create service: %d", rec.Code)
	}
	var svc store.Service
	_ = json.Unmarshal(rec.Body.Bytes(), &svc)

	ingBody := func(svcRef string, port int) map[string]any {
		return map[string]any{
			"name":  "public",
			"class": "nginx",
			"hosts": []map[string]any{{
				"host_template": "{{app}}.example.com",
				"tls":           map[string]any{"enabled": true, "secret_ref": "tls-secret"},
				"rules":         []map[string]any{{"path": "/", "path_type": "Prefix", "service_id": svcRef, "port": port}},
			}},
		}
	}

	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/ingress", principal, ingBody(*svc.ServiceID, 8080))
	if rec.Code != 201 {
		t.Fatalf("create ingress: %d %s", rec.Code, rec.Body.String())
	}
	var ing store.Ingress
	_ = json.Unmarshal(rec.Body.Bytes(), &ing)
	if ing.IngressID == nil || !strings.HasPrefix(*ing.IngressID, "ing-") {
		t.Fatalf("ingress_id = %v, want ing-*", ing.IngressID)
	}

	// unknown service_id → 422; port <= 0 → 422
	if rec = testutil.Do(t, h, http.MethodPost, nsBase+"/ingress", principal, ingBody("svc-nope", 8080)); rec.Code != 422 {
		t.Errorf("unknown service: %d, want 422", rec.Code)
	}
	if rec = testutil.Do(t, h, http.MethodPost, nsBase+"/ingress", principal, ingBody(*svc.ServiceID, 0)); rec.Code != 422 {
		t.Errorf("port 0: %d, want 422", rec.Code)
	}

	// GET list + detail
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/ingress", principal, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), *ing.IngressID) {
		t.Errorf("list: %d %s", rec.Code, rec.Body.String())
	}
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/ingress/"+*ing.IngressID, principal, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "example.com") {
		t.Errorf("detail: %d %s", rec.Code, rec.Body.String())
	}

	// PATCH name
	rec = testutil.Do(t, h, http.MethodPatch, nsBase+"/ingress/"+*ing.IngressID, principal,
		map[string]any{"name": "renamed"})
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var patched store.Ingress
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched.Name != "renamed" || !strings.Contains(string(patched.Hosts), "example.com") {
		t.Errorf("patched = %+v (hosts must survive name PATCH)", patched)
	}

	// DELETE free → 204, then 404
	rec = testutil.Do(t, h, http.MethodDelete, nsBase+"/ingress/"+*ing.IngressID, principal, nil)
	if rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec = testutil.Do(t, h, http.MethodGet, nsBase+"/ingress/"+*ing.IngressID, principal, nil); rec.Code != 404 {
		t.Errorf("after delete: %d, want 404", rec.Code)
	}
}

func TestViewsAndMockStubs(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base+"/namespaces", testutil.WithKey(principal, "ns-v"), map[string]string{"name": "views-env"})
	if rec.Code != 201 {
		t.Fatalf("create ns: %d", rec.Code)
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)
	nsBase := base + "/namespaces/" + ns.ID

	// dataset + database restoring from it + service bound to it
	if rec = testutil.Do(t, h, http.MethodPost, base+"/datasets", testutil.WithKey(principal, "ds-v"), map[string]any{
		"name":     "views-snap",
		"location": map[string]any{"provider": "s3", "bucket": "b", "key": "k.dump"},
	}); rec.Code != 202 {
		t.Fatalf("dataset: %d %s", rec.Code, rec.Body.String())
	}
	var ds struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ds)

	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/databases", testutil.WithKey(principal, "db-v"), map[string]any{
		"name":        "appdb",
		"data_source": map[string]any{"dataset_id": ds.ID, "mode": "restore"},
	})
	if rec.Code != 201 {
		t.Fatalf("database: %d %s", rec.Code, rec.Body.String())
	}
	var dbr store.Database
	_ = json.Unmarshal(rec.Body.Bytes(), &dbr)

	svcBody := serviceFull("widgets")
	svcBody["database_bindings"] = []map[string]any{{"name": "appdb", "database_id": *dbr.DatabaseID}}
	svcBody["dependencies"] = []map[string]any{}
	if rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, svcBody); rec.Code != 201 {
		t.Fatalf("service: %d %s", rec.Code, rec.Body.String())
	}
	var svc store.Service
	_ = json.Unmarshal(rec.Body.Bytes(), &svc)

	// dependencies view
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/dependencies", principal, nil)
	if rec.Code != 200 {
		t.Fatalf("dependencies: %d", rec.Code)
	}
	var deps struct {
		Services []struct {
			ServiceID    string           `json:"service_id"`
			Dependencies []any            `json:"dependencies"`
			Bindings     []map[string]any `json:"database_bindings"`
		} `json:"services"`
		Databases []struct {
			DatabaseID string           `json:"database_id"`
			Bindings   []map[string]any `json:"bindings"`
		} `json:"databases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &deps); err != nil {
		t.Fatal(err)
	}
	if len(deps.Services) != 1 || deps.Services[0].ServiceID != *svc.ServiceID || len(deps.Services[0].Bindings) != 1 {
		t.Errorf("dependencies services = %+v", deps.Services)
	}
	if len(deps.Databases) != 1 || deps.Databases[0].DatabaseID != *dbr.DatabaseID || len(deps.Databases[0].Bindings) != 1 {
		t.Errorf("dependencies databases = %+v", deps.Databases)
	}

	// network-policies view (none applied yet)
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/network-policies", principal, nil)
	if rec.Code != 200 {
		t.Fatalf("network-policies: %d", rec.Code)
	}
	var pols struct {
		NamespaceID string           `json:"namespace_id"`
		Policies    []map[string]any `json:"policies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pols); err != nil {
		t.Fatal(err)
	}
	if pols.NamespaceID != ns.ID || len(pols.Policies) != 0 {
		t.Errorf("network-policies = %+v", pols)
	}

	// mock-status projection
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/mock-status", principal, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "not-deployed (MVP stub)") {
		t.Errorf("mock-status: %d %s", rec.Code, rec.Body.String())
	}
	var mstat struct {
		Services []struct {
			ServiceID   string `json:"service_id"`
			MockEnabled bool   `json:"mock_enabled"`
			PoolClass   string `json:"pool_class"`
		} `json:"services"`
		Databases []struct {
			DatabaseID  string `json:"database_id"`
			MockEnabled bool   `json:"mock_enabled"`
		} `json:"databases"`
		Runtime string `json:"runtime"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mstat); err != nil {
		t.Fatal(err)
	}
	if len(mstat.Services) != 1 || mstat.Services[0].ServiceID != *svc.ServiceID || mstat.Services[0].MockEnabled {
		t.Errorf("mock-status services = %+v", mstat.Services)
	}
	if len(mstat.Databases) != 1 || mstat.Databases[0].DatabaseID != *dbr.DatabaseID {
		t.Errorf("mock-status databases = %+v", mstat.Databases)
	}

	// mocks/reconcile → 202 accepted
	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/mocks/reconcile", principal, nil)
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), "accepted") {
		t.Errorf("mocks/reconcile: %d %s", rec.Code, rec.Body.String())
	}

	// mock-pools at v1 level
	rec = testutil.Do(t, h, http.MethodGet, base+"/mock-pools", principal, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "perf-isolated") ||
		!strings.Contains(rec.Body.String(), "general") {
		t.Errorf("mock-pools: %d %s", rec.Code, rec.Body.String())
	}
}
