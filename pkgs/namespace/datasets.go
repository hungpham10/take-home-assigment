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

const datasetsBasePath = "/api/loadtest/v1/datasets"

// piiClassifications is the allowed pii_classification enum (sql/0005 CHECK).
var piiClassifications = map[string]bool{"masked": true, "synthetic": true, "non_prod_raw": true}

// datasetLocation is the S3 location of the snapshot file.
type datasetLocation struct {
	Provider string `json:"provider"`
	Bucket   string `json:"bucket"`
	Key      string `json:"key"`
	Region   string `json:"region"`
}

// datasetRequest is the POST /datasets body (docs/database-management-api.md).
type datasetRequest struct {
	Name              string          `json:"name"`
	Engine            string          `json:"engine"`
	EngineVersion     string          `json:"engine_version"`
	Location          json.RawMessage `json:"location"`
	RestoreFormat     string          `json:"restore_format"`
	SchemaVersion     string          `json:"schema_version"`
	PIIClassification string          `json:"pii_classification"`
	RestorePolicy     json.RawMessage `json:"restore_policy"`
	RetentionUntil    *time.Time      `json:"retention_until"`
}

// validateDatasetMetadata checks the request-shape rules that do not need a
// file download: engine, PII enum, S3-only location. Returns 422 detail.
func validateDatasetMetadata(req datasetRequest) error {
	if req.Name == "" {
		return specErr{"name is required"}
	}
	if req.Engine != "" && req.Engine != "postgres" {
		return specErr{"engine must be postgres"}
	}
	if req.PIIClassification != "" && !piiClassifications[req.PIIClassification] {
		return specErr{"pii_classification must be masked, synthetic or non_prod_raw"}
	}
	if len(req.Location) == 0 {
		return specErr{"location is required"}
	}
	var loc datasetLocation
	if err := json.Unmarshal(req.Location, &loc); err != nil {
		return specErr{"location must be an object"}
	}
	if loc.Provider != "s3" {
		return specErr{"location.provider must be s3 (no arbitrary URLs)"}
	}
	if loc.Bucket == "" || loc.Key == "" {
		return specErr{"location.bucket and location.key are required"}
	}
	if req.RestoreFormat != "" && req.RestoreFormat != "pg_dump_custom" {
		return specErr{"restore_format must be pg_dump_custom"}
	}
	return nil
}

// validatePIIMetadata is the app-level PII self-check. PLACEHOLDER: the real
// pattern scan (email / VN phone regexes over the dump contents) runs against
// the downloaded file, which the MVP never fetches — here we only enforce that
// the classification metadata itself is present and in the allowed enum. The
// file-content scan lands with the S3 ingestion worker in a later plan.
func validatePIIMetadata(ds store.Dataset) []string {
	var reasons []string
	if !piiClassifications[ds.PIIClassification] {
		reasons = append(reasons, "pii_classification must be masked, synthetic or non_prod_raw")
	}
	if ds.Engine != nil && *ds.Engine != "postgres" {
		reasons = append(reasons, "engine must be postgres")
	}
	return reasons
}

// mountDatasets registers /datasets (tenant-level resource, NOT nested under a
// namespace) flat on r at the /api/loadtest/v1 level.
func mountDatasets(r chi.Router, d Deps) {
	r.With(idempotency.RequireKey).
		Post("/datasets", idempotency.Replay(datasetsBasePath, d.DB, d.createDataset))
	r.Get("/datasets", d.listDatasets)
	r.Get("/datasets/{dataset_id}", d.getDataset)
	r.Patch("/datasets/{dataset_id}", d.patchDataset)
	r.Delete("/datasets/{dataset_id}", d.deleteDataset)
	r.Post("/datasets/{dataset_id}/validate", d.revalidateDataset)
}

// createDataset: owner-only, 202 with a status_url; validation runs
// synchronously in the MVP (IMPORTING → AVAILABLE|INVALID via
// fn_dataset_transition).
func (d Deps) createDataset(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	var req datasetRequest
	if _, _, err := decodeProvided(r, &req); err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "invalid JSON body")
		return
	}
	if err := validateDatasetMetadata(req); err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return
	}
	var loc datasetLocation
	_ = json.Unmarshal(req.Location, &loc)
	ds := store.Dataset{
		TenantID:          p.TenantID,
		OwnerUserID:       strPtr(p.UserID),
		Name:              req.Name,
		SourceRef:         "s3://" + loc.Bucket + "/" + loc.Key,
		PIIClassification: orDefault(req.PIIClassification, "masked"),
		Status:            "IMPORTING",
		RestoreConfig:     rawOr(req.RestorePolicy, "{}"),
		Engine:            strPtr(orDefault(req.Engine, "postgres")),
		EngineVersion:     strPtr(req.EngineVersion),
		Location:          req.Location,
		RestoreFormat:     strPtr(orDefault(req.RestoreFormat, "pg_dump_custom")),
		SchemaVersion:     strPtr(req.SchemaVersion),
		RetentionUntil:    req.RetentionUntil,
		Version:           1,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := d.DB.WithContext(ctx).Create(&ds).Error; err != nil {
		if isUniqueViolation(err) {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "dataset name already exists in tenant")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "create dataset failed")
		return
	}
	d.audit(ctx, r, p, "dataset.create", ds.ID)
	status := d.runDatasetValidation(ctx, &ds)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"id":         ds.ID,
		"status":     status,
		"status_url": datasetsBasePath + "/" + ds.ID,
	})
}

// runDatasetValidation runs the synchronous MVP validation and transitions the
// dataset via fn_dataset_transition (IMPORTING/SUSPENDED → AVAILABLE|INVALID).
// Returns the resulting status.
func (d Deps) runDatasetValidation(ctx context.Context, ds *store.Dataset) string {
	target := "AVAILABLE"
	if reasons := validatePIIMetadata(*ds); len(reasons) > 0 {
		target = "INVALID"
	}
	if ds.Status != target {
		actor := store.ActorJSON(ds.TenantID, deref(ds.OwnerUserID), "")
		if err := store.Transition(ctx, d.DB, "dataset", ds.ID, target, actor, "metadata validation"); err == nil {
			ds.Status = target
		}
	}
	return ds.Status
}

// loadDatasetScoped loads a dataset by id enforcing tenant isolation and
// owner/admin visibility (404 for cross-owner — no existence leak).
func (d Deps) loadDatasetScoped(w http.ResponseWriter, r *http.Request, p authz.Principal, datasetID string) (*store.Dataset, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	q := d.DB.WithContext(ctx).Where("id = ? AND tenant_id = ?", datasetID, p.TenantID)
	if !p.IsAdmin() {
		q = q.Where("owner_user_id = ?", p.UserID)
	}
	var ds store.Dataset
	if err := q.First(&ds).Error; err != nil {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "dataset not found")
		return nil, false
	}
	return &ds, true
}

// listDatasets: owner sees own datasets; admin sees all in the tenant. The
// payload is metadata only — no dump content is ever served.
func (d Deps) listDatasets(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	q := d.DB.WithContext(ctx).Where("tenant_id = ? AND status <> ?", p.TenantID, "DELETED")
	if !p.IsAdmin() {
		q = q.Where("owner_user_id = ?", p.UserID)
	}
	if s := r.URL.Query().Get("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	var rows []store.Dataset
	if err := q.Order("created_at DESC").Limit(100).Find(&rows).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "list datasets failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"datasets": rows, "count": len(rows)})
}

func (d Deps) getDataset(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ds, ok := d.loadDatasetScoped(w, r, p, chi.URLParam(r, "dataset_id"))
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ds)
}

// patchDataset updates name / restore_policy / retention_until (owner-only;
// admin policy-limited is allowed like namespaces). Version bumps on save.
func (d Deps) patchDataset(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ds, ok := d.loadDatasetScoped(w, r, p, chi.URLParam(r, "dataset_id"))
	if !ok {
		return
	}
	var req datasetRequest
	provided, _, err := decodeProvided(r, &req)
	if err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "invalid JSON body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	updates := map[string]any{"version": ds.Version + 1}
	if provided["name"] {
		if req.Name == "" {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "name cannot be empty")
			return
		}
		updates["name"] = req.Name
	}
	if provided["restore_policy"] {
		updates["restore_config"] = []byte(rawOr(req.RestorePolicy, "{}"))
	}
	if provided["retention_until"] {
		updates["retention_until"] = req.RetentionUntil
	}
	if err := d.DB.WithContext(ctx).Model(&store.Dataset{}).Where("id = ?", ds.ID).
		Updates(updates).Error; err != nil {
		if isUniqueViolation(err) {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "dataset name already exists in tenant")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "dataset update failed")
		return
	}
	d.audit(ctx, r, p, "dataset.update", ds.ID)
	_ = d.DB.WithContext(ctx).Where("id = ?", ds.ID).First(ds).Error
	httpx.WriteJSON(w, http.StatusOK, ds)
}

// datasetReferencedCount counts databases in the tenant whose data_source
// points at the dataset (via tbl_namespaces for tenant scoping).
func (d Deps) datasetReferencedCount(ctx context.Context, tenantID, datasetID string) (int64, error) {
	var count int64
	err := d.DB.WithContext(ctx).Model(&store.Database{}).
		Joins("JOIN tbl_namespaces n ON n.id = tbl_databases.namespace_id").
		Where("n.tenant_id = ? AND tbl_databases.data_source->>'dataset_id' = ?", tenantID, datasetID).
		Count(&count).Error
	return count, err
}

// deleteDataset soft-deletes: 409 CONFLICT when a namespace database still
// references it; otherwise AVAILABLE/SUSPENDED/EXPIRED → DELETING → DELETED
// (both transitions via fn_dataset_transition).
func (d Deps) deleteDataset(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ds, ok := d.loadDatasetScoped(w, r, p, chi.URLParam(r, "dataset_id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if refs, err := d.datasetReferencedCount(ctx, p.TenantID, ds.ID); err == nil && refs > 0 {
		httpx.WriteError(w, http.StatusConflict, "CONFLICT",
			"dataset is referenced by a namespace database data_source")
		return
	}
	actor := store.ActorJSON(p.TenantID, p.UserID, p.Role)
	if ds.Status != "DELETING" {
		if err := store.Transition(ctx, d.DB, "dataset", ds.ID, "DELETING", actor, "delete requested"); err != nil {
			httpx.WriteCodeError(w, "INVALID_TRANSITION",
				"dataset cannot be deleted from status "+ds.Status)
			return
		}
	}
	if err := store.Transition(ctx, d.DB, "dataset", ds.ID, "DELETED", actor, "deleted"); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "dataset delete failed")
		return
	}
	d.audit(ctx, r, p, "dataset.delete", ds.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": ds.ID, "status": "DELETED"})
}

// revalidateDataset re-runs the metadata validation (202).
func (d Deps) revalidateDataset(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ds, ok := d.loadDatasetScoped(w, r, p, chi.URLParam(r, "dataset_id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	status := d.runDatasetValidation(ctx, ds)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"id":     ds.ID,
		"status": status,
	})
}
