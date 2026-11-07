package namespace

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hungpham/loadtest-platform/pkgs/store"
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
	d := testDeps(t)
	h := userFixture(t, d, "user")

	// create namespace + service with valid spec
	rec := do(t, d, http.MethodPost, "/", withKey(h, "k1"), map[string]string{"name": "env1"})
	if rec.Code != 201 {
		t.Fatalf("create ns: %d %s", rec.Code, rec.Body.String())
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)

	rec = do(t, d, http.MethodPost, "/"+ns.ID+"/services", h, map[string]any{
		"name": "widgets", "spec": json.RawMessage(widgetSpec),
	})
	if rec.Code != 201 {
		t.Fatalf("create service: %d %s", rec.Code, rec.Body.String())
	}
	var svc store.Service
	_ = json.Unmarshal(rec.Body.Bytes(), &svc)

	// readiness before validate: missing network policies gate — deps empty so pass
	rec = do(t, d, http.MethodGet, "/"+ns.ID+"/readiness", h, nil)
	if rec.Code != 200 {
		t.Fatalf("readiness: %d", rec.Code)
	}

	// validate (DRAFT → VALIDATING → APPLYING)
	rec = do(t, d, http.MethodPost, "/"+ns.ID+"/validate", h, nil)
	if rec.Code != 200 {
		t.Fatalf("validate: %d %s", rec.Code, rec.Body.String())
	}

	// apply (APPLYING → DATA_LOADING → READY + ready_for_loadtest)
	rec = do(t, d, http.MethodPost, "/"+ns.ID+"/apply", withKey(h, "ka1"), nil)
	if rec.Code != 200 {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, d, http.MethodGet, "/"+ns.ID, h, nil)
	var ready store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ready)
	if ready.Status != "READY" || !ready.ReadyForLoadtest {
		t.Errorf("after apply = %+v", ready)
	}

	// idempotent apply replay
	rec2 := do(t, d, http.MethodPost, "/"+ns.ID+"/apply", withKey(h, "ka1"), nil)
	if rec2.Header().Get("Idempotency-Replayed") != "true" || rec2.Code != 200 {
		t.Errorf("apply replay = (%d, replayed=%q)", rec2.Code, rec2.Header().Get("Idempotency-Replayed"))
	}

	// network policy resolved from dependencies
	var policy store.NetworkPolicy
	if err := d.DB.Where("namespace_id = ?", ns.ID).First(&policy).Error; err != nil || !policy.ResolvedFromDeps {
		t.Errorf("network policy = (%v, %v)", err, policy.ResolvedFromDeps)
	}

	// spec change → ready_for_loadtest reset + targets re-indexed
	rec = do(t, d, http.MethodPut, "/"+ns.ID+"/services/widgets/spec", h, map[string]any{
		"spec": json.RawMessage(widgetSpec), // same content → unchanged
	})
	if rec.Code != 200 || !contains(rec.Body.String(), "unchanged") {
		t.Errorf("same-spec upload = (%d, %s)", rec.Code, rec.Body.String())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestValidateFailsOnBadSpec(t *testing.T) {
	d := testDeps(t)
	h := userFixture(t, d, "user")

	rec := do(t, d, http.MethodPost, "/", withKey(h, "kb1"), map[string]string{"name": "env2"})
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)

	rec = do(t, d, http.MethodPost, "/"+ns.ID+"/services", h, map[string]any{
		"name": "broken", "spec": json.RawMessage(brokenSpec),
	})
	if rec.Code != 422 {
		t.Fatalf("bad spec upload: %d %s", rec.Code, rec.Body.String())
	}

	// service WITHOUT spec → validate fails (422), namespace → INVALID path
	rec = do(t, d, http.MethodPost, "/"+ns.ID+"/services", h, map[string]any{"name": "nospec"})
	if rec.Code != 201 {
		t.Fatalf("create nospec service: %d", rec.Code)
	}
	rec = do(t, d, http.MethodPost, "/"+ns.ID+"/validate", h, nil)
	if rec.Code != 422 || !contains(rec.Body.String(), "no OpenAPI spec") {
		t.Errorf("validate with missing spec = (%d, %s)", rec.Code, rec.Body.String())
	}
	// failed validation must mark the namespace INVALID (not silently roll back)
	rec = do(t, d, http.MethodGet, "/"+ns.ID, h, nil)
	var ns2 store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns2)
	if ns2.Status != "INVALID" {
		t.Errorf("status after failed validate = %q, want INVALID", ns2.Status)
	}
}
