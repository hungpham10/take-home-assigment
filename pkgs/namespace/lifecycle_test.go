package namespace_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
