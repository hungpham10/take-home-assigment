package namespace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/serviceopenapi"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// mountServices registers /namespaces/{id}/services (plan 17: full service
// definition, paths addressed by SERVER-GENERATED service_id) flat on r
// (composition-root pattern — no nested router).
func mountServices(r chi.Router, d Deps) {
	r.Post("/namespaces/{id}/services", d.createService)
	r.Get("/namespaces/{id}/services", d.listServices)
	r.Get("/namespaces/{id}/services/{service_id}", d.getService)
	r.Put("/namespaces/{id}/services/{service_id}", d.putService)
	r.Patch("/namespaces/{id}/services/{service_id}", d.patchService)
	r.Delete("/namespaces/{id}/services/{service_id}", d.deleteService)
	r.Get("/namespaces/{id}/services/{service_id}/openapi", d.getServiceOpenAPI)
	r.Get("/namespaces/{id}/services/{service_id}/apis", d.getServiceAPIs)
}

// --- definition validation (shared by create / PUT / PATCH / create-with-spec) ----

// mockDef is the subset of the service mock JSONB we validate (docs §5).
type mockDef struct {
	Enabled    bool   `json:"enabled"`
	PoolClass  string `json:"pool_class"`
	Operations []struct {
		OperationID string `json:"operation_id"`
	} `json:"operations"`
}

// bindingDef is one database_bindings[] entry. database_id existence is
// validated in plan 18 PR C — here only the shape (binding name) is required.
// `name` is canonical; `binding` is an accepted alias (documented in
// docs/service-management-api.md & docs/namespace-management-api.md) so
// existing clients keep working.
type bindingDef struct {
	Name       string `json:"name"`
	Binding    string `json:"binding"`
	DatabaseID string `json:"database_id"`
}

// dependencyRef is one dependencies[] entry (service_id reference).
type dependencyRef struct {
	ServiceID string `json:"service_id"`
}

// validateMockShape checks the mock profile JSON shape (must be an object when
// set). Shared by service (plans/17) and database (plans/18) definitions.
func validateMockShape(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var m mockDef
	if err := json.Unmarshal(raw, &m); err != nil {
		return specErr{"mock must be an object"}
	}
	return nil
}

// validateServiceDefinition checks the non-openapi parts of a service
// definition: mock operations must match operation IDs from the parsed spec,
// database_bindings entries must carry a binding name, dependencies[] must
// reference a service_id that exists in the namespace (DB query) or is
// addressable within the same request (refs — used by create-with-spec where
// sibling services may not be persisted yet). Package-level so spec.go's
// create-with-spec flow reuses it (plan 17 §"Helpers").
func validateServiceDefinition(db *gorm.DB, nsID string, s serviceSpec, summary *serviceopenapi.Summary, refs map[string]bool) error {
	// mock.operations[].operation_id must exist in the parsed spec targets.
	if len(s.Mock) > 0 {
		var m mockDef
		if err := json.Unmarshal(s.Mock, &m); err != nil {
			return specErr{"mock must be an object"}
		}
		opIDs := map[string]bool{}
		if summary != nil {
			for _, t := range summary.Targets {
				opIDs[t.OperationID] = true
				opIDs[t.ID] = true
			}
		}
		for _, op := range m.Operations {
			if !opIDs[op.OperationID] {
				return specErr{"mock.operations[] references unknown operation_id " + op.OperationID}
			}
		}
	}
	// database_bindings: shape only (binding name required; database_id
	// existence comes with plan 18 PR C). Accept `name` (canonical) or
	// `binding` (documented alias).
	if len(s.DatabaseBindings) > 0 {
		var bindings []bindingDef
		if err := json.Unmarshal(s.DatabaseBindings, &bindings); err != nil {
			return specErr{"database_bindings must be an array"}
		}
		for _, b := range bindings {
			if b.Name == "" && b.Binding == "" {
				return specErr{"database_bindings[].name (or binding) is required"}
			}
		}
	}
	// dependencies[]: service_id must exist in the namespace or the same spec.
	if len(s.Dependencies) > 0 {
		var deps []dependencyRef
		if err := json.Unmarshal(s.Dependencies, &deps); err != nil {
			return specErr{"dependencies must be an array"}
		}
		for _, dep := range deps {
			if dep.ServiceID == "" {
				return specErr{"dependencies[].service_id is required"}
			}
			if refs[dep.ServiceID] {
				continue
			}
			if db != nil && nsID != "" {
				var count int64
				if err := db.Model(&store.Service{}).
					Where("namespace_id = ? AND service_id = ?", nsID, dep.ServiceID).
					Count(&count).Error; err != nil {
					return err
				}
				if count > 0 {
					continue
				}
			}
			return specErr{"dependencies[] references unknown service " + dep.ServiceID}
		}
	}
	return nil
}

// --- body decoding -----------------------------------------------------------------

// decodedServiceSpec carries the definition plus the set of top-level keys the
// client actually provided (needed for PATCH merge semantics).
type decodedServiceSpec struct {
	serviceSpec
	provided map[string]bool
	raw      map[string]json.RawMessage
}

// RawField returns the raw JSON of a provided top-level key ("" when absent).
func (d decodedServiceSpec) RawField(key string) []byte { return d.raw[key] }

// decodeProvided reads the request body ONCE and decodes both the typed body
// and the provided-key set (PATCH merge semantics). Shared by services,
// databases, datasets and ingress bodies.
func decodeProvided(r *http.Request, v any) (provided map[string]bool, raw map[string]json.RawMessage, err error) {
	provided, raw = map[string]bool{}, map[string]json.RawMessage{}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return provided, raw, err
	}
	if len(body) == 0 {
		return provided, raw, nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return provided, raw, err
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return provided, raw, err
	}
	for k := range raw {
		provided[k] = true
	}
	return provided, raw, nil
}

// decodeServiceSpec reads the request body ONCE and decodes both the
// definition and the provided-key set.
func decodeServiceSpec(r *http.Request) (decodedServiceSpec, error) {
	out := decodedServiceSpec{provided: map[string]bool{}, raw: map[string]json.RawMessage{}}
	provided, raw, err := decodeProvided(r, &out.serviceSpec)
	out.provided, out.raw = provided, raw
	return out, err
}

// --- create -----------------------------------------------------------------------

// createService POSTs a full service definition (plans/17). service_id is
// SERVER-generated ("svc-"+rand8); client-supplied service_id → 422.
// Backward compat: legacy {"name","spec"} shape still works.
func (d Deps) createService(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	body, err := decodeServiceSpec(r)
	if err != nil || body.Name == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return
	}
	if body.ServiceID != "" {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED",
			"service_id is server-generated")
		return
	}
	if err := normalizeOpenAPI(&body.serviceSpec); err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return
	}
	if body.OpenAPI != nil && body.OpenAPI.Content == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "openapi.content is required when openapi is set")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	svcID, err := randomID()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "id generation failed")
		return
	}
	svc := store.Service{
		NamespaceID:      ns.ID,
		ServiceID:        strPtr("svc-" + svcID),
		Name:             body.Name,
		Description:      body.Description,
		Deploy:           rawOr(body.Deploy, "{}"),
		Mock:             rawOr(body.Mock, "{}"),
		DatabaseBindings: rawOr(body.DatabaseBindings, "[]"),
		Dependencies:     rawOr(body.Dependencies, "[]"),
		ParsedSummary:    store.JSONBytes(map[string]any{}),
		Version:          1,
	}
	var summary *serviceopenapi.Summary
	if body.OpenAPI != nil {
		summary, err = serviceopenapi.ValidateAndIndex([]byte(body.OpenAPI.Content))
		if err != nil {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", err.Error())
			return
		}
		svc.OpenAPISpec = []byte(body.OpenAPI.Content)
		svc.SpecHash = store.StrPtr(serviceopenapi.SpecHash([]byte(body.OpenAPI.Content)))
		svc.ParsedSummary = summary.Marshal()
	}
	// A single POST can only reference services already persisted in the ns.
	if err := validateServiceDefinition(d.DB, ns.ID, body.serviceSpec, summary, map[string]bool{}); err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return
	}
	if err := d.DB.WithContext(ctx).Create(&svc).Error; err != nil {
		if isUniqueViolation(err) {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "service name already exists")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "create service failed")
		return
	}
	d.audit(ctx, r, p, "service.create", svc.ID)
	httpx.WriteJSON(w, http.StatusCreated, svc)
}

// normalizeOpenAPI maps the legacy raw "spec" field onto openapi.content.
func normalizeOpenAPI(s *serviceSpec) error {
	if len(s.Spec) > 0 {
		if s.OpenAPI != nil && s.OpenAPI.Content != "" {
			return specErr{"openapi and spec are mutually exclusive"}
		}
		s.OpenAPI = &openapiSpec{Format: "json", ContentType: "application/json", Content: string(s.Spec)}
	}
	return nil
}

// --- list / load ------------------------------------------------------------------

func (d Deps) listServices(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	var services []store.Service
	if err := d.DB.Where("namespace_id = ?", ns.ID).Order("name").Find(&services).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "list services failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"services": services, "count": len(services)})
}

// loadServiceByID loads the namespace (tenant/owner scoped) then the service
// by SERVER-GENERATED service_id (plan 17: paths no longer use the name).
func (d Deps) loadServiceByID(w http.ResponseWriter, r *http.Request, p authz.Principal) (*store.Service, bool) {
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return nil, false
	}
	var svc store.Service
	if err := d.DB.Where("namespace_id = ? AND service_id = ?", ns.ID, chi.URLParam(r, "service_id")).
		First(&svc).Error; err != nil {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "service not found")
		return nil, false
	}
	return &svc, true
}

func (d Deps) getService(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	svc, ok := d.loadServiceByID(w, r, p)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, svc)
}

// --- PUT (full replacement) / PATCH (partial merge) --------------------------------

// putService replaces the whole definition (plans/17). When openapi content
// changed (spec_hash comparison) it is revalidated, parsed_summary + spec_hash
// are refreshed and namespace ready_for_loadtest resets to false.
func (d Deps) putService(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	svc, ok := d.loadServiceByID(w, r, p)
	if !ok {
		return
	}
	body, err := decodeServiceSpec(r)
	if err != nil || body.Name == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return
	}
	if body.ServiceID != "" && body.ServiceID != deref(svc.ServiceID) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED",
			"service_id is server-generated and cannot be changed")
		return
	}
	if !d.applyServiceUpdate(w, r, p, svc, body, true) {
		return // error response already written
	}
	httpx.WriteJSON(w, http.StatusOK, svc)
}

// patchService merges only the provided fields; openapi provided → revalidate.
func (d Deps) patchService(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	svc, ok := d.loadServiceByID(w, r, p)
	if !ok {
		return
	}
	body, err := decodeServiceSpec(r)
	if err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "invalid JSON body")
		return
	}
	if body.ServiceID != "" && body.ServiceID != deref(svc.ServiceID) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED",
			"service_id is server-generated and cannot be changed")
		return
	}
	if !d.applyServiceUpdate(w, r, p, svc, body, false) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, svc)
}

// applyServiceUpdate is the shared PUT/PATCH core. fullReplace=true means PUT
// (missing definition fields reset to defaults, openapi omitted clears the
// spec); false means PATCH merge (only provided fields change). When openapi
// content changed it is revalidated and readiness is reset. Returns false when
// an error response was written.
func (d Deps) applyServiceUpdate(w http.ResponseWriter, r *http.Request, p authz.Principal,
	svc *store.Service, body decodedServiceSpec, fullReplace bool,
) bool {
	if err := normalizeOpenAPI(&body.serviceSpec); err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return false
	}
	// pick resolves a JSONB field: client value wins (PATCH provided / PUT
	// always), PATCH falls back to the stored value, defaults otherwise.
	pick := func(cur json.RawMessage, key, def string) json.RawMessage {
		if body.provided[key] {
			if len(body.RawField(key)) > 0 {
				return json.RawMessage(body.RawField(key))
			}
			return json.RawMessage(def)
		}
		if fullReplace {
			return json.RawMessage(def)
		}
		if len(cur) == 0 {
			return json.RawMessage(def)
		}
		return cur
	}
	next := serviceSpec{
		Name:             body.Name,
		Description:      body.Description,
		Deploy:           pick(svc.Deploy, "deploy", "{}"),
		Mock:             pick(svc.Mock, "mock", "{}"),
		DatabaseBindings: pick(svc.DatabaseBindings, "database_bindings", "[]"),
		Dependencies:     pick(svc.Dependencies, "dependencies", "[]"),
		OpenAPI:          body.OpenAPI,
	}
	if !fullReplace {
		if next.Name == "" {
			next.Name = svc.Name
		}
		if !body.provided["description"] {
			next.Description = svc.Description
		}
		if next.OpenAPI == nil && len(svc.OpenAPISpec) > 0 {
			next.OpenAPI = &openapiSpec{Content: string(svc.OpenAPISpec)}
		}
	}

	// Spec handling: revalidate only when the content actually changed.
	var summary *serviceopenapi.Summary
	var newSpec []byte
	var newHash string
	specChanged := false
	switch {
	case next.OpenAPI != nil && next.OpenAPI.Content != "":
		newSpec = []byte(next.OpenAPI.Content)
		newHash = serviceopenapi.SpecHash(newSpec)
		if svc.SpecHash == nil || *svc.SpecHash != newHash {
			var err error
			summary, err = serviceopenapi.ValidateAndIndex(newSpec)
			if err != nil {
				httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", err.Error())
				return false
			}
			specChanged = true
		}
	case fullReplace:
		// Full replacement without openapi clears the spec.
		if len(svc.OpenAPISpec) > 0 {
			specChanged = true
		}
	}
	// Mock validation runs against the NEW parsed summary (or the stored one
	// when the spec content is unchanged).
	if summary == nil && len(svc.ParsedSummary) > 0 && len(svc.OpenAPISpec) > 0 {
		summary = &serviceopenapi.Summary{}
		_ = json.Unmarshal(svc.ParsedSummary, summary)
	}
	if err := validateServiceDefinition(d.DB, svc.NamespaceID, next, summary, map[string]bool{}); err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return false
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	updates := map[string]any{
		"name":              next.Name,
		"description":       next.Description,
		"deploy":            []byte(next.Deploy),
		"mock":              []byte(next.Mock),
		"database_bindings": []byte(next.DatabaseBindings),
		"dependencies":      []byte(next.Dependencies),
		"version":           svc.Version + 1,
	}
	switch {
	case specChanged && newSpec != nil:
		updates["openapi_spec"] = newSpec
		updates["spec_hash"] = newHash
		updates["parsed_summary"] = summary.Marshal()
	case specChanged:
		updates["openapi_spec"] = gorm.Expr("NULL")
		updates["spec_hash"] = gorm.Expr("NULL")
		updates["parsed_summary"] = store.JSONBytes(map[string]any{})
	}
	err := d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&store.Service{}).Where("id = ?", svc.ID).Updates(updates).Error; err != nil {
			if isUniqueViolation(err) {
				return specErr{"service name already exists"}
			}
			return err
		}
		if specChanged {
			// Spec change invalidates readiness: reset ready_for_loadtest.
			return tx.Model(&store.Namespace{}).
				Where("id = ? AND ready_for_loadtest = ?", svc.NamespaceID, true).
				Update("ready_for_loadtest", false).Error
		}
		return nil
	})
	if err != nil {
		var se specErr
		if errors.As(err, &se) {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", se.msg)
			return false
		}
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "service update failed")
		return false
	}
	if specChanged {
		d.audit(ctx, r, p, "service.spec.update", svc.ID)
	} else {
		d.audit(ctx, r, p, "service.update", svc.ID)
	}
	// Reflect the new state on the returned row.
	if err := d.DB.WithContext(ctx).Where("id = ?", svc.ID).First(svc).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "service reload failed")
		return false
	}
	return true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- delete ------------------------------------------------------------------------

// deleteService refuses (409 CONFLICT) when the service is referenced by
// another service's dependencies[] or by an ingress host rule. Loadtest
// targets (tbl_load_tests.targets JSONB) are intentionally NOT checked here.
func (d Deps) deleteService(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	svc, ok := d.loadServiceByID(w, r, p)
	if !ok {
		return
	}
	if reason := d.serviceDeleteConflict(svc); reason != "" {
		httpx.WriteError(w, http.StatusConflict, "CONFLICT", reason)
		return
	}
	if err := d.DB.Delete(&store.Service{}, "id = ?", svc.ID).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "delete service failed")
		return
	}
	d.audit(r.Context(), r, p, "service.delete", svc.ID)
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// serviceDeleteConflict returns a conflict reason ("" when deletable).
func (d Deps) serviceDeleteConflict(svc *store.Service) string {
	var siblings []store.Service
	_ = d.DB.Where("namespace_id = ? AND id <> ?", svc.NamespaceID, svc.ID).Find(&siblings).Error
	sid := deref(svc.ServiceID)
	for _, s := range siblings {
		var deps []dependencyRef
		if err := json.Unmarshal(s.Dependencies, &deps); err == nil {
			for _, dep := range deps {
				if dep.ServiceID == sid {
					return "service " + sid + " is referenced by service " + deref(s.ServiceID) + " dependencies"
				}
			}
		}
	}
	var ingresses []store.Ingress
	_ = d.DB.Where("namespace_id = ?", svc.NamespaceID).Find(&ingresses).Error
	for _, ing := range ingresses {
		for _, rule := range ingressRulesOf(ing.Hosts) {
			if rule.ServiceID == sid || rule.ServiceID == svc.Name {
				return "service " + sid + " is referenced by ingress " + deref(ing.IngressID) + " host rules"
			}
		}
	}
	return ""
}

// --- openapi / apis sub-resources ---------------------------------------------------

// getServiceOpenAPI returns the stored OpenAPI content with Content-Type per
// the spec format (JSON body → application/json, else YAML). 404 when the
// service has no spec.
func (d Deps) getServiceOpenAPI(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	svc, ok := d.loadServiceByID(w, r, p)
	if !ok {
		return
	}
	if len(svc.OpenAPISpec) == 0 {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "service has no OpenAPI spec")
		return
	}
	ct := "application/json"
	if !json.Valid(svc.OpenAPISpec) {
		ct = "application/yaml"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(svc.OpenAPISpec)
}

// getServiceAPIs returns the parsed load-test targets from parsed_summary.
func (d Deps) getServiceAPIs(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	svc, ok := d.loadServiceByID(w, r, p)
	if !ok {
		return
	}
	var summary serviceopenapi.Summary
	_ = json.Unmarshal(svc.ParsedSummary, &summary)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"service_id": deref(svc.ServiceID),
		"count":      len(summary.Targets),
		"apis":       summary.Targets,
	})
}
