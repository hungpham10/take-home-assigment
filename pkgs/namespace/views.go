package namespace

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// mockRuntimeStub is the honest MVP answer for every mock projection: there is
// no mock-service runtime yet (plans/18 §5 — stub rõ ràng, mock-service phase sau).
const mockRuntimeStub = "not-deployed (MVP stub)"

// mockPoolClass is the mock_pool_class default when a mock profile omits it.
const mockPoolClassDefault = "general"

// mockPoolOf extracts (enabled, pool_class) from a stored mock JSONB.
func mockPoolOf(raw json.RawMessage) (bool, string) {
	var m mockDef
	if json.Unmarshal(raw, &m) != nil {
		return false, ""
	}
	if !m.Enabled {
		return false, ""
	}
	if m.PoolClass == "" {
		return true, mockPoolClassDefault
	}
	return true, m.PoolClass
}

// mountViews registers the read-only projections + mock stubs under
// /namespaces/{id}/... flat on r.
func mountViews(r chi.Router, d Deps) {
	r.Get("/namespaces/{id}/dependencies", d.dependencies)
	r.Get("/namespaces/{id}/network-policies", d.networkPolicies)
	r.Get("/namespaces/{id}/mock-status", d.mockStatus)
	r.Post("/namespaces/{id}/mocks/reconcile", d.mocksReconcile)
}

// MountV1 registers the tenant-level routes that live at /api/loadtest/v1
// (datasets + mock-pools). Call from the server composition root inside the
// RequirePrincipal group.
func MountV1(r chi.Router, d Deps) {
	mountDatasets(r, d)
	r.Get("/mock-pools", d.mockPools)
}

// dependencyServiceView is one services[] entry of GET /dependencies.
type dependencyServiceView struct {
	ServiceID        string          `json:"service_id"`
	Name             string          `json:"name"`
	Dependencies     []dependencyRef `json:"dependencies"`
	DatabaseBindings []bindingDef    `json:"database_bindings"`
}

// databaseBindingView is one databases[] entry of GET /dependencies: the
// database plus the services bound to it.
type databaseBindingView struct {
	DatabaseID string       `json:"database_id"`
	Name       string       `json:"name"`
	Bindings   []bindingDef `json:"bindings"`
}

// dependencies resolves each service's dependencies[] entries and the
// database_bindings graph (service → binding name → database).
func (d Deps) dependencies(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	var services []store.Service
	if err := d.DB.Where("namespace_id = ?", ns.ID).Order("name").Find(&services).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "dependencies query failed")
		return
	}
	var databases []store.Database
	if err := d.DB.Where("namespace_id = ?", ns.ID).Order("name").Find(&databases).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "dependencies query failed")
		return
	}
	svcViews := make([]dependencyServiceView, 0, len(services))
	bindingsByDB := map[string][]bindingDef{}
	for _, svc := range services {
		view := dependencyServiceView{
			ServiceID:        deref(svc.ServiceID),
			Name:             svc.Name,
			Dependencies:     []dependencyRef{},
			DatabaseBindings: []bindingDef{},
		}
		_ = json.Unmarshal(svc.Dependencies, &view.Dependencies)
		_ = json.Unmarshal(svc.DatabaseBindings, &view.DatabaseBindings)
		for _, b := range view.DatabaseBindings {
			bindingsByDB[b.DatabaseID] = append(bindingsByDB[b.DatabaseID],
				bindingDef{Name: b.Name, DatabaseID: view.ServiceID})
		}
		svcViews = append(svcViews, view)
	}
	dbViews := make([]databaseBindingView, 0, len(databases))
	for _, dbRow := range databases {
		bindings := bindingsByDB[deref(dbRow.DatabaseID)]
		if bindings == nil {
			bindings = []bindingDef{}
		}
		dbViews = append(dbViews, databaseBindingView{
			DatabaseID: deref(dbRow.DatabaseID),
			Name:       dbRow.Name,
			Bindings:   bindings,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"namespace_id": ns.ID,
		"services":     svcViews,
		"databases":    dbViews,
	})
}

// networkPolicies returns the resolved policy rows (spec + observed_generation).
func (d Deps) networkPolicies(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	var policies []store.NetworkPolicy
	if err := d.DB.Where("namespace_id = ?", ns.ID).Order("created_at").Find(&policies).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "network policy query failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"namespace_id": ns.ID,
		"policies":     policies,
	})
}

// mockStatus projects service/database mock metadata (no runtime — stub).
func (d Deps) mockStatus(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	var services []store.Service
	_ = d.DB.Where("namespace_id = ?", ns.ID).Order("name").Find(&services).Error
	var databases []store.Database
	_ = d.DB.Where("namespace_id = ?", ns.ID).Order("name").Find(&databases).Error

	type mockStatusEntry struct {
		ServiceID   string `json:"service_id"`
		MockEnabled bool   `json:"mock_enabled"`
		PoolClass   string `json:"pool_class"`
	}
	svcEntries := make([]mockStatusEntry, 0, len(services))
	for _, svc := range services {
		enabled, pool := mockPoolOf(svc.Mock)
		svcEntries = append(svcEntries, mockStatusEntry{
			ServiceID: deref(svc.ServiceID), MockEnabled: enabled, PoolClass: pool,
		})
	}
	type dbMockStatusEntry struct {
		DatabaseID  string `json:"database_id"`
		MockEnabled bool   `json:"mock_enabled"`
		PoolClass   string `json:"pool_class"`
	}
	dbEntries := make([]dbMockStatusEntry, 0, len(databases))
	for _, dbRow := range databases {
		enabled, pool := mockPoolOf(dbRow.Mock)
		dbEntries = append(dbEntries, dbMockStatusEntry{
			DatabaseID: deref(dbRow.DatabaseID), MockEnabled: enabled, PoolClass: pool,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"namespace_id": ns.ID,
		"services":     svcEntries,
		"databases":    dbEntries,
		"runtime":      mockRuntimeStub,
	})
}

// mocksReconcile accepts a reconcile request. STUB: the mock-service runtime
// does not exist in the MVP, so nothing is enqueued — 202 with an explicit
// accepted marker.
func (d Deps) mocksReconcile(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	d.audit(r.Context(), r, p, "mocks.reconcile", ns.ID)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// mockPools is the mock-pool catalog. STUB: fixed classes until the
// mock-service runtime exists (docs/database-management-api.md §mock-pools).
func (d Deps) mockPools(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"pools": []map[string]any{
			{
				"pool_class":    "general",
				"description":   "shared mock instances for functional environments",
				"max_instances": 10,
			},
			{
				"pool_class":    "perf-isolated",
				"description":   "dedicated mock instances for performance runs",
				"max_instances": 5,
			},
		},
		"runtime": mockRuntimeStub,
	})
}
