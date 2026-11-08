package namespace_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

const widgetSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "Widgets", "version": "1.0.0"},
  "paths": {
    "/widgets": {
      "get": {"operationId": "listWidgets", "responses": {"200": {"description": "ok"}}},
      "post": {"responses": {"201": {"description": "created"}}}
    }
  }
}`

const brokenSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "Broken", "version": "1.0.0"},
  "paths": {"/items/{id}": {"get": {"responses": {"200": {"description": "ok"}}}}}
}`

func TestValidateApplyReadinessLifecycle(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	// create namespace + service with valid spec
	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "k1"), map[string]string{"name": "env1"})
	if rec.Code != 201 {
		t.Fatalf("create ns: %d %s", rec.Code, rec.Body.String())
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)

	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns.ID+"/services", principal, map[string]any{
		"name": "widgets", "spec": json.RawMessage(widgetSpec),
	})
	if rec.Code != 201 {
		t.Fatalf("create service: %d %s", rec.Code, rec.Body.String())
	}
	var svc store.Service
	_ = json.Unmarshal(rec.Body.Bytes(), &svc)

	// readiness before validate: missing network policies gate — deps empty so pass
	rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns.ID+"/readiness", principal, nil)
	if rec.Code != 200 {
		t.Fatalf("readiness: %d", rec.Code)
	}

	// validate (DRAFT → VALIDATING → APPLYING)
	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns.ID+"/validate", principal, nil)
	if rec.Code != 200 {
		t.Fatalf("validate: %d %s", rec.Code, rec.Body.String())
	}

	// apply (APPLYING → DATA_LOADING → READY + ready_for_loadtest)
	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns.ID+"/apply", testutil.WithKey(principal, "ka1"), nil)
	if rec.Code != 200 {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns.ID, principal, nil)
	var ready store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ready)
	if ready.Status != "READY" || !ready.ReadyForLoadtest {
		t.Errorf("after apply = %+v", ready)
	}

	// idempotent apply replay
	rec2 := testutil.Do(t, h, http.MethodPost, base+"/"+ns.ID+"/apply", testutil.WithKey(principal, "ka1"), nil)
	if rec2.Header().Get("Idempotency-Replayed") != "true" || rec2.Code != 200 {
		t.Errorf("apply replay = (%d, replayed=%q)", rec2.Code, rec2.Header().Get("Idempotency-Replayed"))
	}

	// network policy resolved from dependencies
	var policy store.NetworkPolicy
	if err := db.Where("namespace_id = ?", ns.ID).First(&policy).Error; err != nil || !policy.ResolvedFromDeps {
		t.Errorf("network policy = (%v, %v)", err, policy.ResolvedFromDeps)
	}

	// spec upload via service_id-based PUT (plan 17): same content → no
	// revalidation, the updated service row is returned.
	rec = testutil.Do(t, h, http.MethodPut, base+"/"+ns.ID+"/services/"+*svc.ServiceID, principal, map[string]any{
		"name":    "widgets",
		"openapi": map[string]any{"format": "json", "content_type": "application/json", "content": widgetSpec},
	})
	if rec.Code != 200 {
		t.Errorf("same-spec PUT = (%d, %s)", rec.Code, rec.Body.String())
	}
}

func TestValidateFailsOnBadSpec(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "kb1"), map[string]string{"name": "env2"})
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)

	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns.ID+"/services", principal, map[string]any{
		"name": "broken", "spec": json.RawMessage(brokenSpec),
	})
	if rec.Code != 422 {
		t.Fatalf("bad spec upload: %d %s", rec.Code, rec.Body.String())
	}

	// service WITHOUT spec → validate fails (422), namespace → INVALID path
	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns.ID+"/services", principal, map[string]any{"name": "nospec"})
	if rec.Code != 201 {
		t.Fatalf("create nospec service: %d", rec.Code)
	}
	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns.ID+"/validate", principal, nil)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "no OpenAPI spec") {
		t.Errorf("validate with missing spec = (%d, %s)", rec.Code, rec.Body.String())
	}
	// failed validation must mark the namespace INVALID (not silently roll back)
	rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns.ID, principal, nil)
	var ns2 store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns2)
	if ns2.Status != "INVALID" {
		t.Errorf("status after failed validate = %q, want INVALID", ns2.Status)
	}
}

// TestApplyK8SAsyncHandoff — plans/21 PR B: with the cluster wired (K8S=true)
// POST /apply must NOT reach READY synchronously. It bumps desired_generation
// and leaves the status at APPLYING for the Env Reconciler + Status Watcher.
func TestApplyK8SAsyncHandoff(t *testing.T) {
	db := testutil.TestDB(t)
	h := testutil.NewServer(t, testutil.ServerDeps{DB: db, Now: time.Now, K8S: true})
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "k1"), map[string]string{"name": "envk8s"})
	if rec.Code != 201 {
		t.Fatalf("create ns: %d %s", rec.Code, rec.Body.String())
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)

	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns.ID+"/services", principal, map[string]any{
		"name": "widgets", "spec": json.RawMessage(widgetSpec),
	})
	if rec.Code != 201 {
		t.Fatalf("create service: %d %s", rec.Code, rec.Body.String())
	}

	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns.ID+"/validate", principal, nil)
	if rec.Code != 200 {
		t.Fatalf("validate: %d %s", rec.Code, rec.Body.String())
	}

	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns.ID+"/apply", testutil.WithKey(principal, "ka1"), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status            string `json:"status"`
		DesiredGeneration int64  `json:"desired_generation"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status != "APPLYING" || resp.DesiredGeneration != 1 {
		t.Fatalf("hand-off response %+v", resp)
	}

	// DB: desired_generation bumped, still APPLYING, not ready, no fake
	// observed_generation on the policy (watcher owns that now).
	var row store.Namespace
	if err := db.Where("id = ?", ns.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "APPLYING" || row.DesiredGeneration != 1 || row.ReadyForLoadtest {
		t.Fatalf("ns row: status=%s desired=%d ready=%v", row.Status, row.DesiredGeneration, row.ReadyForLoadtest)
	}
	var policy store.NetworkPolicy
	if err := db.Where("namespace_id = ?", ns.ID).First(&policy).Error; err != nil {
		t.Fatal(err)
	}
	if policy.ObservedGeneration != 0 {
		t.Fatalf("policy observed_generation %d, want 0 (watcher owns it)", policy.ObservedGeneration)
	}
}

// TestReadinessProjectionBacked — plans/22: với cluster wired (K8S=true),
// GET /readiness đọc tbl_component_status + blocking_reasons thay vì compute
// readinessGates on-the-fly (docs/namespace-management-api.md §3).
func TestReadinessProjectionBacked(t *testing.T) {
	db := testutil.TestDB(t)
	h := testutil.NewServer(t, testutil.ServerDeps{DB: db, Now: time.Now, K8S: true})
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "k1"), map[string]string{"name": "envproj"})
	if rec.Code != 201 {
		t.Fatalf("create ns: %d %s", rec.Code, rec.Body.String())
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)

	// Watcher projection state: service READY (gen 2), data_load PENDING.
	now := time.Now().UTC()
	rows := []store.ComponentStatus{
		{NamespaceID: ns.ID, ComponentType: "service", ComponentID: "svc-1", Required: true,
			DesiredGeneration: 2, ObservedGeneration: 2, Phase: "READY", ObservedAt: now},
		{NamespaceID: ns.ID, ComponentType: "data_load", ComponentID: "-", Required: true,
			DesiredGeneration: 2, ObservedGeneration: 1, Phase: "PENDING", Reason: store.StrPtr("restore_job_not_implemented"), ObservedAt: now},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	blocking, _ := json.Marshal([]string{"data_load/-: restore_job_not_implemented"})
	if err := db.Model(&store.Namespace{}).Where("id = ?", ns.ID).Updates(map[string]any{
		"blocking_reasons":   blocking,
		"status_observed_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns.ID+"/readiness", principal, nil)
	if rec.Code != 200 {
		t.Fatalf("readiness: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status          string         `json:"status"`
		Ready           bool           `json:"ready"`
		BlockingReasons []string       `json:"blocking_reasons"`
		Components      map[string]any `json:"components"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("decode: %s", rec.Body.String())
	}
	if _, ok := resp.Components["services"]; !ok {
		t.Fatalf("projection blocks missing: %v", resp.Components)
	}
	if len(resp.BlockingReasons) == 0 {
		t.Fatal("blocking_reasons not projection-backed")
	}
	if resp.Ready {
		t.Fatal("ready=true despite PENDING component")
	}

	// Sanity: the payload must NOT come from readinessGates — readinessGates
	// would report "no services registered" because no tbl_services rows
	// exist; the projection reports READY for svc-1.
	svcBlock := resp.Components["services"].(map[string]any)
	if svcBlock["ready"].(float64) != 1 {
		t.Fatalf("service block not from projection: %v", svcBlock)
	}
}
