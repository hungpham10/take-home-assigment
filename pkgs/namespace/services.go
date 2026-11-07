package namespace

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/serviceopenapi"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// mountServices registers /namespaces/{id}/services (OpenAPI component CRUD)
// flat on r (composition-root pattern — no nested router).
func mountServices(r chi.Router, d Deps) {
	r.Post("/namespaces/{id}/services", d.createService)
	r.Get("/namespaces/{id}/services", d.listServices)
	r.Get("/namespaces/{id}/services/{svc}", d.getService)
	r.Put("/namespaces/{id}/services/{svc}/spec", d.putSpec)
	r.Delete("/namespaces/{id}/services/{svc}", d.deleteService)
}

func (d Deps) createService(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	var body struct {
		Name string          `json:"name"`
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	svc := store.Service{NamespaceID: ns.ID, Name: body.Name, ParsedSummary: store.JSONBytes(map[string]any{})}
	if len(body.Spec) > 0 {
		summary, err := serviceopenapi.ValidateAndIndex(body.Spec)
		if err != nil {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", err.Error())
			return
		}
		svc.OpenAPISpec = body.Spec
		svc.SpecHash = store.StrPtr(serviceopenapi.SpecHash(body.Spec))
		svc.ParsedSummary = summary.Marshal()
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

func (d Deps) loadService(w http.ResponseWriter, r *http.Request, p authz.Principal) (*store.Service, bool) {
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return nil, false
	}
	var svc store.Service
	if err := d.DB.Where("namespace_id = ? AND name = ?", ns.ID, chi.URLParam(r, "svc")).
		First(&svc).Error; err != nil {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "service not found")
		return nil, false
	}
	return &svc, true
}

func (d Deps) getService(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	svc, ok := d.loadService(w, r, p)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, svc)
}

// putSpec uploads/replaces the OpenAPI spec (revalidate on change).
func (d Deps) putSpec(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	svc, ok := d.loadService(w, r, p)
	if !ok {
		return
	}
	var body struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Spec) == 0 {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "spec is required")
		return
	}
	summary, err := serviceopenapi.ValidateAndIndex(body.Spec)
	if err != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	newHash := serviceopenapi.SpecHash(body.Spec)
	if svc.SpecHash != nil && *svc.SpecHash == newHash {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "unchanged", "hash": newHash})
		return
	}
	// Spec change invalidates readiness: namespace falls back to VALIDATING.
	if err := d.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&store.Service{}).
			Where("id = ?", svc.ID).
			Updates(map[string]any{
				"openapi_spec":   []byte(body.Spec),
				"spec_hash":      newHash,
				"parsed_summary": summary.Marshal(),
				"version":        svc.Version + 1,
			}).Error; err != nil {
			return err
		}
		// ready_for_loadtest=false until re-validated + re-applied.
		return tx.Model(&store.Namespace{}).
			Where("id = ? AND ready_for_loadtest = ?", svc.NamespaceID, true).
			Update("ready_for_loadtest", false).Error
	}); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "spec update failed")
		return
	}
	d.audit(ctx, r, p, "service.spec.update", svc.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "updated", "hash": newHash, "op_count": summary.OpCount})
}

func (d Deps) deleteService(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	svc, ok := d.loadService(w, r, p)
	if !ok {
		return
	}
	if err := d.DB.Delete(&store.Service{}, "id = ?", svc.ID).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "delete service failed")
		return
	}
	d.audit(r.Context(), r, p, "service.delete", svc.ID)
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}
