package namespace_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

// Tests for plan 17 (PR B): service API by service_id, mock/binding/dependency
// validation, delete conflicts, and the components-shaped readiness report.

// serviceFull returns a full service definition body (2 operations).
func serviceFull(name string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": "widget service",
		"deploy":      map[string]any{"image": "widgets:1", "replicas": 2},
		"openapi": map[string]any{
			"format": "json", "version": "3.0.0", "content_type": "application/json",
			"content": widgetSpec,
		},
	}
}

func TestINT_SVC_01_ServiceAPIByServiceID(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "sv1"), map[string]string{"name": "svc-env"})
	if rec.Code != 201 {
		t.Fatalf("create ns: %d %s", rec.Code, rec.Body.String())
	}
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)
	nsBase := base + "/" + ns.ID

	// create-with-spec: service_id is server-generated
	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, serviceFull("widgets"))
	if rec.Code != 201 {
		t.Fatalf("create service: %d %s", rec.Code, rec.Body.String())
	}
	var svc store.Service
	_ = json.Unmarshal(rec.Body.Bytes(), &svc)
	if !strings.HasPrefix(*svc.ServiceID, "svc-") {
		t.Fatalf("service_id = %q, want svc-*", *svc.ServiceID)
	}

	// client-supplied service_id → 422
	body := serviceFull("other")
	body["service_id"] = "svc-custom"
	if rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, body); rec.Code != 422 {
		t.Errorf("client service_id: %d, want 422", rec.Code)
	}

	// GET detail by service_id
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/services/"+*svc.ServiceID, principal, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "widget service") {
		t.Errorf("get by service_id: %d %s", rec.Code, rec.Body.String())
	}

	// PATCH description only (merge — spec preserved)
	rec = testutil.Do(t, h, http.MethodPatch, nsBase+"/services/"+*svc.ServiceID, principal,
		map[string]any{"description": "patched"})
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var patched store.Service
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched.Description != "patched" || len(patched.OpenAPISpec) == 0 || patched.SpecHash == nil {
		t.Errorf("patched = %+v (spec must survive a description-only PATCH)", patched)
	}

	// PUT full replacement with a NEW spec (revalidates, new hash)
	newSpec := `{"openapi":"3.0.0","info":{"title":"Widgets2","version":"2"},
		"paths":{"/v2/widgets":{"get":{"operationId":"listV2","responses":{"200":{"description":"ok"}}}}}}`
	rec = testutil.Do(t, h, http.MethodPut, nsBase+"/services/"+*svc.ServiceID, principal, map[string]any{
		"name":    "widgets",
		"openapi": map[string]any{"format": "json", "content_type": "application/json", "content": newSpec},
	})
	if rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	var replaced store.Service
	_ = json.Unmarshal(rec.Body.Bytes(), &replaced)
	if replaced.SpecHash == nil || *replaced.SpecHash == *svc.SpecHash {
		t.Errorf("spec_hash not refreshed on PUT: %v vs %v", replaced.SpecHash, svc.SpecHash)
	}

	// GET /openapi returns the new content with a JSON content type
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/services/"+*svc.ServiceID+"/openapi", principal, nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(rec.Body.String(), "Widgets2") {
		t.Errorf("openapi endpoint: (%d, %q, %s)", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}

	// GET /apis returns the parsed targets (1 op in the replacement spec)
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/services/"+*svc.ServiceID+"/apis", principal, nil)
	if rec.Code != 200 {
		t.Fatalf("apis: %d", rec.Code)
	}
	var apis struct {
		ServiceID string `json:"service_id"`
		Count     int    `json:"count"`
		APIs      []struct {
			ID string `json:"id"`
		} `json:"apis"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &apis); err != nil {
		t.Fatal(err)
	}
	if apis.ServiceID != *svc.ServiceID || apis.Count != 1 || len(apis.APIs) != 1 || apis.APIs[0].ID != "listV2" {
		t.Errorf("apis = %+v", apis)
	}

	// 2-op spec via create → /apis count = 2
	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, serviceFull("two-op"))
	var svc2 store.Service
	_ = json.Unmarshal(rec.Body.Bytes(), &svc2)
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/services/"+*svc2.ServiceID+"/apis", principal, nil)
	var apis2 struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &apis2)
	if apis2.Count != 2 {
		t.Errorf("2-op spec apis count = %d, want 2", apis2.Count)
	}

	// unknown service_id → 404
	if rec = testutil.Do(t, h, http.MethodGet, nsBase+"/services/svc-nope", principal, nil); rec.Code != 404 {
		t.Errorf("unknown service_id: %d, want 404", rec.Code)
	}
}

func TestINT_SVC_MockValidation(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "mv1"), map[string]string{"name": "mock-env"})
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)
	nsBase := base + "/" + ns.ID

	// mock operation referencing an UNKNOWN operation_id → 422
	bad := serviceFull("mocked")
	bad["mock"] = map[string]any{
		"enabled":    true,
		"operations": []map[string]any{{"operation_id": "nopeOp"}},
	}
	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, bad)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "nopeOp") {
		t.Errorf("unknown mock op: (%d, %s), want 422", rec.Code, rec.Body.String())
	}

	// known operation_id → 201
	good := serviceFull("mocked")
	good["mock"] = map[string]any{
		"enabled":    true,
		"operations": []map[string]any{{"operation_id": "listWidgets"}},
	}
	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, good)
	if rec.Code != 201 {
		t.Errorf("known mock op: %d %s, want 201", rec.Code, rec.Body.String())
	}

	// database_bindings without a binding name → 422 (shape; existence is PR C)
	badBindings := serviceFull("bound")
	badBindings["database_bindings"] = []map[string]any{{"database_id": "db-x"}}
	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, badBindings)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "name") {
		t.Errorf("binding shape: (%d, %s), want 422", rec.Code, rec.Body.String())
	}

	// dependencies referencing an unknown service → 422
	badDeps := serviceFull("dependent")
	badDeps["dependencies"] = []map[string]any{{"service_id": "svc-ghost"}}
	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, badDeps)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "svc-ghost") {
		t.Errorf("unknown dependency: (%d, %s), want 422", rec.Code, rec.Body.String())
	}
}

func TestINT_SVC_DeleteConflict(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "dc1"), map[string]string{"name": "del-env"})
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)
	nsBase := base + "/" + ns.ID

	mk := func(name string, deps []map[string]any) string {
		body := serviceFull(name)
		if deps != nil {
			body["dependencies"] = deps
		}
		rec := testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, body)
		if rec.Code != 201 {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body.String())
		}
		var svc store.Service
		_ = json.Unmarshal(rec.Body.Bytes(), &svc)
		return *svc.ServiceID
	}
	first := mk("first", nil)
	second := mk("second", []map[string]any{{"service_id": first}})

	// referenced by another service's dependencies → 409 CONFLICT
	rec = testutil.Do(t, h, http.MethodDelete, nsBase+"/services/"+first, principal, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"CONFLICT"`) {
		t.Errorf("delete referenced: (%d, %s), want 409 CONFLICT", rec.Code, rec.Body.String())
	}
	// unreferenced → 204
	rec = testutil.Do(t, h, http.MethodDelete, nsBase+"/services/"+second, principal, nil)
	if rec.Code != 204 {
		t.Errorf("delete unreferenced: %d, want 204", rec.Code)
	}
	// now the first is deletable
	rec = testutil.Do(t, h, http.MethodDelete, nsBase+"/services/"+first, principal, nil)
	if rec.Code != 204 {
		t.Errorf("delete after ref gone: %d, want 204", rec.Code)
	}
}

func TestINT_NS_29_ReadinessComponents(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "rd1"), map[string]string{"name": "rd-env"})
	var ns store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns)
	nsBase := base + "/" + ns.ID

	rec = testutil.Do(t, h, http.MethodPost, nsBase+"/services", principal, serviceFull("widgets"))
	if rec.Code != 201 {
		t.Fatalf("create service: %d", rec.Code)
	}

	// components shape is always present
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/readiness", principal, nil)
	if rec.Code != 200 {
		t.Fatalf("readiness: %d", rec.Code)
	}
	var rep struct {
		NamespaceID      string         `json:"namespace_id"`
		Status           string         `json:"status"`
		ReadyForLoadtest bool           `json:"ready_for_loadtest"`
		CheckedAt        string         `json:"checked_at"`
		Components       map[string]any `json:"components"`
		Ready            bool           `json:"ready"`
		BlockingReasons  []string       `json:"blocking_reasons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"services", "databases", "data_load", "ingress", "dependencies", "network_policy", "mocks"} {
		if _, ok := rep.Components[key]; !ok {
			t.Errorf("readiness missing component %q (got %v)", key, rep.Components)
		}
	}
	if rep.NamespaceID != ns.ID || rep.CheckedAt == "" {
		t.Errorf("readiness header fields = %+v", rep)
	}
	// services ready, no blocking gates yet; the network_policy row only
	// appears after apply → applied=false.
	if !strings.Contains(rec.Body.String(), `"applied":false`) {
		t.Errorf("expected network_policy.applied=false: %s", rec.Body.String())
	}
	if !rep.Ready {
		t.Errorf("ready should be true when no gates block: %s", rec.Body.String())
	}
	// A legacy tbl_dependencies row without a resolved policy → blocking.
	if err := db.Create(&store.Dependency{NamespaceID: ns.ID, Type: "mock", Config: []byte("{}")}).Error; err != nil {
		t.Fatal(err)
	}
	rec = testutil.Do(t, h, http.MethodGet, nsBase+"/readiness", principal, nil)
	if !strings.Contains(rec.Body.String(), "network policy not applied") {
		t.Errorf("expected network_policy blocking with unresolved deps: %s", rec.Body.String())
	}

	// database with data_source pointing at a MISSING dataset → databases gate
	// blocks with a per-database reason.
	rec = testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "rd2"), map[string]string{"name": "rd-env-db"})
	var ns2 store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns2)
	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns2.ID+"/services", principal, serviceFull("widgets"))
	if rec.Code != 201 {
		t.Fatalf("create service 2: %d", rec.Code)
	}
	dbRow := store.Database{
		NamespaceID: ns2.ID, Name: "main", Engine: "postgres", Operator: "cloudnative-pg",
		DataSource:    []byte(`{"dataset_id":"ds-missing","mode":"restore"}`),
		RestorePolicy: store.JSONBytes(map[string]any{}),
		Ephemeral:     store.JSONBytes(map[string]any{}),
		Mock:          store.JSONBytes(map[string]any{}),
		Profile:       store.JSONBytes(map[string]any{}),
		Version:       1,
	}
	if err := db.Create(&dbRow).Error; err != nil {
		t.Fatal(err)
	}
	rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns2.ID+"/readiness", principal, nil)
	if !strings.Contains(rec.Body.String(), "database widgets-main: dataset ds-missing not available") &&
		!strings.Contains(rec.Body.String(), "ds-missing") {
		t.Errorf("databases gate blocking reason missing: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"required":true`) {
		t.Errorf("data_load.required should be true with restore mode: %s", rec.Body.String())
	}

	// mock-enabled service → mocks blocking; validate passes; apply → 409
	// INSUFFICIENT_MOCK_CAPACITY (mock runtime is an apply-time gate, MVP stub).
	rec = testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "rd3"), map[string]string{"name": "rd-env-mock"})
	var ns3 store.Namespace
	_ = json.Unmarshal(rec.Body.Bytes(), &ns3)
	body := serviceFull("mocked")
	body["mock"] = map[string]any{
		"enabled":    true,
		"operations": []map[string]any{{"operation_id": "listWidgets"}},
	}
	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns3.ID+"/services", principal, body)
	if rec.Code != 201 {
		t.Fatalf("create mock service: %d %s", rec.Code, rec.Body.String())
	}
	rec = testutil.Do(t, h, http.MethodGet, base+"/"+ns3.ID+"/readiness", principal, nil)
	if !strings.Contains(rec.Body.String(), "mock runtime not available (MVP stub)") {
		t.Errorf("mocks blocking reason missing: %s", rec.Body.String())
	}
	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns3.ID+"/validate", principal, nil)
	if rec.Code != 200 {
		t.Fatalf("validate with mock: %d %s (mock capacity is apply-time)", rec.Code, rec.Body.String())
	}
	rec = testutil.Do(t, h, http.MethodPost, base+"/"+ns3.ID+"/apply", testutil.WithKey(principal, "rd3a"), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "INSUFFICIENT_MOCK_CAPACITY") {
		t.Errorf("apply with mock: (%d, %s), want 409 INSUFFICIENT_MOCK_CAPACITY", rec.Code, rec.Body.String())
	}
}
