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
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// ingressRule is one host rule (docs/database-management-api.md §ingress).
type ingressRule struct {
	Path      string `json:"path"`
	PathType  string `json:"path_type"`
	ServiceID string `json:"service_id"`
	Port      int    `json:"port"`
}

// ingressHost is one hosts[] entry.
type ingressHost struct {
	HostTemplate string          `json:"host_template"`
	TLS          json.RawMessage `json:"tls"`
	Rules        []ingressRule   `json:"rules"`
}

// ingressRequest is the POST/PUT/PATCH /namespaces/{id}/ingress body.
type ingressRequest struct {
	IngressID string          `json:"ingress_id"`
	Name      string          `json:"name"`
	Class     string          `json:"class"`
	Hosts     json.RawMessage `json:"hosts"`
}

// ingressRulesOf decodes stored tbl_ingress.hosts JSONB into rules. It accepts
// the hosts[]-of-objects shape (plans/18) and falls back to the legacy flat
// rules array (pre-0010 rows / create-with-spec bodies).
func ingressRulesOf(raw json.RawMessage) []ingressRule {
	if len(raw) == 0 {
		return nil
	}
	var hosts []ingressHost
	if err := json.Unmarshal(raw, &hosts); err == nil {
		var out []ingressRule
		for _, h := range hosts {
			out = append(out, h.Rules...)
		}
		return out
	}
	var flat []ingressRule
	if err := json.Unmarshal(raw, &flat); err == nil {
		return flat
	}
	return nil
}

// validateIngressHosts checks every host rule: service_id must exist in the
// namespace (by service_id or name — both are addressable refs, same rule as
// dependencies[]) and port must be positive.
func validateIngressHosts(db *gorm.DB, nsID string, hosts json.RawMessage, refs map[string]bool) error {
	if len(hosts) == 0 {
		return nil
	}
	var parsed []ingressHost
	if err := json.Unmarshal(hosts, &parsed); err != nil {
		return specErr{"hosts must be an array of {host_template, tls, rules}"}
	}
	// Refs within the same request (create-with-spec) bypass the DB lookup.
	if refs == nil {
		refs = map[string]bool{}
	}
	exists := func(id string) bool {
		if refs[id] {
			return true
		}
		if db == nil || nsID == "" {
			return false
		}
		var count int64
		if err := db.Model(&store.Service{}).
			Where("namespace_id = ? AND (service_id = ? OR name = ?)", nsID, id, id).
			Count(&count).Error; err != nil {
			return false
		}
		return count > 0
	}
	for _, h := range parsed {
		for _, rule := range h.Rules {
			if rule.ServiceID == "" {
				return specErr{"hosts[].rules[].service_id is required"}
			}
			if !exists(rule.ServiceID) {
				return specErr{"hosts[].rules[] references unknown service " + rule.ServiceID}
			}
			if rule.Port <= 0 {
				return specErr{"hosts[].rules[].port must be positive"}
			}
		}
	}
	return nil
}

// mountIngress registers /namespaces/{id}/ingress[/{ingress_id}] flat on r.
func mountIngress(r chi.Router, d Deps) {
	r.Post("/namespaces/{id}/ingress", d.createIngress)
	r.Get("/namespaces/{id}/ingress", d.listIngress)
	r.Get("/namespaces/{id}/ingress/{ingress_id}", d.getIngress)
	r.Put("/namespaces/{id}/ingress/{ingress_id}", d.putIngress)
	r.Patch("/namespaces/{id}/ingress/{ingress_id}", d.patchIngress)
	r.Delete("/namespaces/{id}/ingress/{ingress_id}", d.deleteIngress)
}

// createIngress creates an ingress resource; ingress_id is SERVER-generated
// ("ing-"+rand8), client-supplied ingress_id → 422.
func (d Deps) createIngress(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	var req ingressRequest
	provided, _, err := decodeProvided(r, &req)
	if err != nil || req.Name == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return
	}
	if req.IngressID != "" {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED",
			"ingress_id is server-generated")
		return
	}
	ingID, err := randomID()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "id generation failed")
		return
	}
	row, err := d.buildIngress(ns.ID, nil, req, provided, true, map[string]bool{})
	if err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return
	}
	row.IngressID = strPtr("ing-" + ingID)
	if err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return
	}
	row.Paths = store.JSONBytes([]any{})
	row.Version = 1
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := d.DB.WithContext(ctx).Create(&row).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "create ingress failed")
		return
	}
	d.audit(ctx, r, p, "ingress.create", row.ID)
	httpx.WriteJSON(w, http.StatusCreated, row)
}

// buildIngress validates + materializes an ingress row. fullReplace=true (PUT /
// POST) resets omitted fields to defaults; false (PATCH) keeps the stored
// values (cur) for keys the client omitted. cur is nil for create.
func (d Deps) buildIngress(nsID string, cur *store.Ingress, req ingressRequest, provided map[string]bool, fullReplace bool, refs map[string]bool) (store.Ingress, error) {
	row := store.Ingress{NamespaceID: nsID}
	if cur != nil && !fullReplace {
		row = *cur
	}
	if provided["class"] || fullReplace {
		row.Class = orDefault(req.Class, "nginx")
	}
	if req.Class != "" && req.Class != "nginx" {
		return row, specErr{"class must be nginx"}
	}
	if provided["name"] || fullReplace {
		if req.Name == "" {
			return row, specErr{"name is required"}
		}
		row.Name = req.Name
	}
	if provided["hosts"] || fullReplace {
		if err := validateIngressHosts(d.DB, nsID, req.Hosts, refs); err != nil {
			return row, err
		}
		row.Hosts = rawOr(req.Hosts, "[]")
	}
	return row, nil
}

func (d Deps) listIngress(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	var rows []store.Ingress
	if err := d.DB.Where("namespace_id = ?", ns.ID).Order("name").Find(&rows).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "list ingress failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ingress": rows, "count": len(rows)})
}

// loadIngressByID loads the namespace (tenant/owner scoped) then the ingress by
// SERVER-GENERATED ingress_id (404-safe, no existence leak).
func (d Deps) loadIngressByID(w http.ResponseWriter, r *http.Request, p authz.Principal) (*store.Ingress, bool) {
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return nil, false
	}
	var row store.Ingress
	if err := d.DB.Where("namespace_id = ? AND ingress_id = ?", ns.ID, chi.URLParam(r, "ingress_id")).
		First(&row).Error; err != nil {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "ingress not found")
		return nil, false
	}
	return &row, true
}

func (d Deps) getIngress(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadIngressByID(w, r, p)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row)
}

// applyIngressUpdate is the shared PUT/PATCH core; returns false when an error
// response was written.
func (d Deps) applyIngressUpdate(w http.ResponseWriter, r *http.Request, p authz.Principal,
	row *store.Ingress, req ingressRequest, provided map[string]bool, fullReplace bool,
) bool {
	if req.IngressID != "" && req.IngressID != deref(row.IngressID) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED",
			"ingress_id is server-generated and cannot be changed")
		return false
	}
	next, err := d.buildIngress(row.NamespaceID, row, req, provided, fullReplace, nil)
	if err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := d.DB.WithContext(ctx).Model(&store.Ingress{}).Where("id = ?", row.ID).Updates(map[string]any{
		"name":    next.Name,
		"class":   next.Class,
		"hosts":   []byte(next.Hosts),
		"version": row.Version + 1,
	}).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "ingress update failed")
		return false
	}
	d.audit(ctx, r, p, "ingress.update", row.ID)
	if err := d.DB.WithContext(ctx).Where("id = ?", row.ID).First(row).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "ingress reload failed")
		return false
	}
	return true
}

func (d Deps) putIngress(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadIngressByID(w, r, p)
	if !ok {
		return
	}
	var req ingressRequest
	provided, _, err := decodeProvided(r, &req)
	if err != nil || req.Name == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return
	}
	if !d.applyIngressUpdate(w, r, p, row, req, provided, true) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row)
}

func (d Deps) patchIngress(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadIngressByID(w, r, p)
	if !ok {
		return
	}
	var req ingressRequest
	provided, _, err := decodeProvided(r, &req)
	if err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "invalid JSON body")
		return
	}
	if !d.applyIngressUpdate(w, r, p, row, req, provided, false) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row)
}

// deleteIngress removes the ingress. No delete conflicts exist yet (nothing
// references an ingress in the MVP).
func (d Deps) deleteIngress(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadIngressByID(w, r, p)
	if !ok {
		return
	}
	if err := d.DB.Delete(&store.Ingress{}, "id = ?", row.ID).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "delete ingress failed")
		return
	}
	d.audit(r.Context(), r, p, "ingress.delete", row.ID)
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}
