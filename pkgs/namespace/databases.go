package namespace

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/idempotency"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// databaseRequest is the POST/PUT/PATCH /namespaces/{id}/databases body
// (docs/database-management-api.md §database).
type databaseRequest struct {
	DatabaseID    string          `json:"database_id"`
	Name          string          `json:"name"`
	Engine        string          `json:"engine"`
	Operator      string          `json:"operator"`
	EngineVersion string          `json:"engine_version"`
	Version       string          `json:"version"` // legacy alias (create-with-spec shape)
	Config        json.RawMessage `json:"config"`
	InitScriptRef string          `json:"init_script_ref"`
	DataSource    json.RawMessage `json:"data_source"`
	RestorePolicy json.RawMessage `json:"restore_policy"`
	Ephemeral     json.RawMessage `json:"ephemeral"`
	Mock          json.RawMessage `json:"mock"`
	SecretRef     string          `json:"secret_ref"`
}

// engineVersionOf resolves the engine version from engine_version or the
// legacy "version" alias (create-with-spec uses "version").
func (req databaseRequest) engineVersionOf() string {
	if req.EngineVersion != "" {
		return req.EngineVersion
	}
	return req.Version
}

// validateDatabaseDefinition checks the definition parts that need the DB:
// engine must be postgres, data_source.dataset_id (when set) must be an
// AVAILABLE dataset in the tenant, mock profile shape. Shared by POST/PUT/
// PATCH and (via validateSpecComponents' subset) create-with-spec.
func (d Deps) validateDatabaseDefinition(tenantID string, req databaseRequest, provided map[string]bool, fullReplace bool) error {
	if provided["engine"] || fullReplace {
		if req.Engine != "" && req.Engine != "postgres" {
			return specErr{"engine must be postgres"}
		}
	}
	if provided["data_source"] || fullReplace {
		if err := datasetAvailable(d.DB, tenantID, req.DataSource); err != nil {
			return err
		}
	}
	if provided["mock"] || fullReplace {
		if err := validateMockShape(req.Mock); err != nil {
			return err
		}
	}
	return nil
}

// mountDatabases registers /namespaces/{id}/databases[/{database_id}] flat on
// r (composition-root pattern — no nested router).
func mountDatabases(r chi.Router, d Deps) {
	r.With(idempotency.RequireKey).
		Post("/namespaces/{id}/databases", idempotency.Replay("/api/loadtest/v1/databases", d.DB, d.createDatabase))
	r.Get("/namespaces/{id}/databases", d.listDatabases)
	r.Get("/namespaces/{id}/databases/{database_id}", d.getDatabase)
	r.Put("/namespaces/{id}/databases/{database_id}", d.putDatabase)
	r.Patch("/namespaces/{id}/databases/{database_id}", d.patchDatabase)
	r.Delete("/namespaces/{id}/databases/{database_id}", d.deleteDatabase)
}

// createDatabase creates a database row; database_id is SERVER-generated
// ("db-"+rand8), client-supplied database_id → 422.
func (d Deps) createDatabase(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	var req databaseRequest
	provided, _, err := decodeProvided(r, &req)
	if err != nil || req.Name == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return
	}
	if req.DatabaseID != "" {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED",
			"database_id is server-generated")
		return
	}
	if err := d.validateDatabaseDefinition(p.TenantID, req, provided, true); err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return
	}
	dbID, err := randomID()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "id generation failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row := databaseRow(ns.ID, "db-"+dbID, req)
	if err := d.DB.WithContext(ctx).Create(&row).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "create database failed")
		return
	}
	d.audit(ctx, r, p, "database.create", row.ID)
	httpx.WriteJSON(w, http.StatusCreated, row)
}

// databaseRow materializes a store.Database from the request (defaults applied).
func databaseRow(nsID, dbID string, req databaseRequest) store.Database {
	return store.Database{
		NamespaceID:   nsID,
		DatabaseID:    strPtr(dbID),
		Name:          req.Name,
		Engine:        orDefault(req.Engine, "postgres"),
		Operator:      orDefault(req.Operator, "cloudnative-pg"),
		EngineVersion: strPtr(req.engineVersionOf()),
		Config:        rawOr(req.Config, "{}"),
		InitScriptRef: strPtr(req.InitScriptRef),
		DataSource:    rawOr(req.DataSource, "{}"),
		RestorePolicy: rawOr(req.RestorePolicy, "{}"),
		Ephemeral:     rawOr(req.Ephemeral, "{}"),
		Mock:          rawOr(req.Mock, "{}"),
		SecretRef:     strPtr(req.SecretRef),
		Profile:       store.JSONBytes(map[string]any{}),
		Version:       1,
	}
}

func (d Deps) listDatabases(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	var rows []store.Database
	if err := d.DB.Where("namespace_id = ?", ns.ID).Order("name").Find(&rows).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "list databases failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"databases": rows, "count": len(rows)})
}

// loadDatabaseByID loads the namespace (tenant/owner scoped) then the database
// by SERVER-GENERATED database_id (404-safe, no existence leak).
func (d Deps) loadDatabaseByID(w http.ResponseWriter, r *http.Request, p authz.Principal) (*store.Database, bool) {
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return nil, false
	}
	var row store.Database
	if err := d.DB.Where("namespace_id = ? AND database_id = ?", ns.ID, chi.URLParam(r, "database_id")).
		First(&row).Error; err != nil {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "database not found")
		return nil, false
	}
	return &row, true
}

func (d Deps) getDatabase(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadDatabaseByID(w, r, p)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row)
}

// applyDatabaseUpdate is the shared PUT/PATCH core; returns false when an error
// response was written. Revalidates engine/data_source/mock when provided (PUT
// always). The version column (optimistic-concurrency counter) is bumped.
func (d Deps) applyDatabaseUpdate(w http.ResponseWriter, r *http.Request, p authz.Principal,
	row *store.Database, req databaseRequest, provided map[string]bool, fullReplace bool,
) bool {
	if req.DatabaseID != "" && req.DatabaseID != deref(row.DatabaseID) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED",
			"database_id is server-generated and cannot be changed")
		return false
	}
	if err := d.validateDatabaseDefinition(p.TenantID, req, provided, fullReplace); err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return false
	}
	// pick resolves a field: client value wins (PATCH provided / PUT always),
	// PATCH falls back to the stored value, defaults otherwise.
	storage := map[string]func(store.Database) any{
		"name":           func(c store.Database) any { return c.Name },
		"engine":         func(_ store.Database) any { return orDefault(req.Engine, "postgres") },
		"operator":       func(_ store.Database) any { return orDefault(req.Operator, "cloudnative-pg") },
		"engine_version": func(_ store.Database) any { return strPtr(req.engineVersionOf()) },
		"config":         func(_ store.Database) any { return []byte(rawOr(req.Config, "{}")) },
		"init_script_ref": func(_ store.Database) any {
			return strPtr(req.InitScriptRef)
		},
		"data_source":    func(_ store.Database) any { return []byte(rawOr(req.DataSource, "{}")) },
		"restore_policy": func(_ store.Database) any { return []byte(rawOr(req.RestorePolicy, "{}")) },
		"ephemeral":      func(_ store.Database) any { return []byte(rawOr(req.Ephemeral, "{}")) },
		"mock":           func(_ store.Database) any { return []byte(rawOr(req.Mock, "{}")) },
		"secret_ref":     func(_ store.Database) any { return strPtr(req.SecretRef) },
	}
	stored := map[string]func() any{
		"name":            func() any { return row.Name },
		"engine":          func() any { return row.Engine },
		"operator":        func() any { return row.Operator },
		"engine_version":  func() any { return row.EngineVersion },
		"config":          func() any { return row.Config },
		"init_script_ref": func() any { return row.InitScriptRef },
		"data_source":     func() any { return row.DataSource },
		"restore_policy":  func() any { return row.RestorePolicy },
		"ephemeral":       func() any { return row.Ephemeral },
		"mock":            func() any { return row.Mock },
		"secret_ref":      func() any { return row.SecretRef },
	}
	updates := map[string]any{"version": row.Version + 1}
	for _, key := range []string{"name", "engine", "operator", "engine_version", "config",
		"init_script_ref", "data_source", "restore_policy", "ephemeral", "mock", "secret_ref"} {
		switch {
		case provided[key]:
			updates[key] = storage[key](*row)
		case fullReplace:
			updates[key] = storage[key](store.Database{})
		default:
			updates[key] = stored[key]()
		}
	}
	if updates["name"] == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := d.DB.WithContext(ctx).Model(&store.Database{}).Where("id = ?", row.ID).
		Updates(updates).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "database update failed")
		return false
	}
	d.audit(ctx, r, p, "database.update", row.ID)
	if err := d.DB.WithContext(ctx).Where("id = ?", row.ID).First(row).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "database reload failed")
		return false
	}
	return true
}

func (d Deps) putDatabase(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadDatabaseByID(w, r, p)
	if !ok {
		return
	}
	var req databaseRequest
	provided, _, err := decodeProvided(r, &req)
	if err != nil || req.Name == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return
	}
	if !d.applyDatabaseUpdate(w, r, p, row, req, provided, true) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row)
}

func (d Deps) patchDatabase(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadDatabaseByID(w, r, p)
	if !ok {
		return
	}
	var req databaseRequest
	provided, _, err := decodeProvided(r, &req)
	if err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "invalid JSON body")
		return
	}
	if !d.applyDatabaseUpdate(w, r, p, row, req, provided, false) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row)
}

// deleteDatabase refuses (409 CONFLICT) when any service in the namespace
// references it via database_bindings[].database_id.
func (d Deps) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadDatabaseByID(w, r, p)
	if !ok {
		return
	}
	if blocker := d.databaseDeleteConflict(row); blocker != "" {
		httpx.WriteError(w, http.StatusConflict, "CONFLICT", blocker)
		return
	}
	if err := d.DB.Delete(&store.Database{}, "id = ?", row.ID).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "delete database failed")
		return
	}
	d.audit(r.Context(), r, p, "database.delete", row.ID)
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// databaseDeleteConflict returns a conflict reason ("" when deletable).
func (d Deps) databaseDeleteConflict(row *store.Database) string {
	dbID := deref(row.DatabaseID)
	var services []store.Service
	_ = d.DB.Where("namespace_id = ?", row.NamespaceID).Find(&services).Error
	for _, svc := range services {
		var bindings []bindingDef
		if json.Unmarshal(svc.DatabaseBindings, &bindings) != nil {
			continue
		}
		for _, b := range bindings {
			if b.DatabaseID == dbID {
				return "database " + dbID + " is referenced by service " + deref(svc.ServiceID) + " database_bindings"
			}
		}
	}
	return ""
}
