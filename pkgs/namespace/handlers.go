// Package namespace implements /api/loadtest/v1/namespaces — namespace CRUD
// with Idempotency-Key, ETag optimistic concurrency and DB-driven state
// transitions (fn_namespace_transition; app never UPDATEs status directly).
package namespace

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/idempotency"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Deps carries the handlers' dependencies.
type Deps struct {
	DB  *gorm.DB
	Now func() time.Time
}

// Mount registers the namespace routes (namespace CRUD, services, lifecycle)
// FLAT on the passed-in router (server composition root applies
// authz.RequirePrincipal). All patterns carry the /namespaces prefix.
func Mount(r chi.Router, d Deps) {
	r.With(idempotency.RequireKey).
		Post("/namespaces", idempotency.Replay("/api/loadtest/v1/namespaces", d.DB, d.create))
	r.Get("/namespaces", d.listNamespaces)
	r.Get("/namespaces/{id}", d.getNamespace)
	r.Patch("/namespaces/{id}", d.updateNamespace)
	// Soft delete: DRAFT/INVALID/READY/DEGRADED → DELETING (reaper finishes it).
	r.Delete("/namespaces/{id}", d.deleteNamespace)
	r.Get("/namespaces/{id}/events", d.events)
	// NOTE: all routes are registered flat on r — a chi subtree at the dynamic
	// param path Mount("/{id}/...") swallows the exact GET /{id} route.
	mountServices(r, d)
	mountDatabases(r, d)
	mountIngress(r, d)
	mountViews(r, d)
	mountLifecycle(r, d)
}

// mountLifecycle registers validate / apply / readiness flat on r.
func mountLifecycle(r chi.Router, d Deps) {
	prefix := "/namespaces/{id}"
	r.Post(prefix+"/validate", d.validateNamespace)
	r.With(idempotency.RequireKey).
		Post(prefix+"/apply", idempotency.Replay("/api/loadtest/v1/apply", d.DB, d.applyNamespace))
	r.Get(prefix+"/readiness", d.readiness)
}

// --- handlers --------------------------------------------------------------------

// createRequest is the POST /namespaces body (docs/namespace-management-api.md
// §create). Spec is optional — nil Spec keeps the legacy metadata-only create.
type createRequest struct {
	Name        string         `json:"name"`
	Slug        string         `json:"slug"`
	Description string         `json:"description"`
	Visibility  string         `json:"visibility"`
	TTL         *time.Time     `json:"ttl_expires_at"`
	Apply       bool           `json:"apply"`
	Spec        *namespaceSpec `json:"spec"`
}

func (d Deps) create(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return
	}
	if req.Visibility == "" {
		req.Visibility = "tenant"
	}
	if req.Visibility != "tenant" && req.Visibility != "public" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "visibility must be tenant or public")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Legacy shape: no spec → metadata-only namespace row.
	if req.Spec == nil {
		ns := store.Namespace{
			TenantID:      p.TenantID,
			OwnerUserID:   p.UserID,
			Name:          req.Name,
			Slug:          strPtr(req.Slug),
			Visibility:    strPtr(req.Visibility),
			Description:   req.Description,
			Status:        "DRAFT",
			Version:       1,
			TTLExpiresAt:  req.TTL,
			CleanupPolicy: store.JSONBytes(map[string]any{}),
		}
		if err := d.DB.WithContext(ctx).Create(&ns).Error; err != nil {
			if isUniqueViolation(err) {
				httpx.WriteCodeError(w, "VALIDATION_FAILED", "namespace name already exists in tenant")
				return
			}
			httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "create namespace failed")
			return
		}
		d.audit(ctx, r, p, "namespace.create", ns.ID)
		writeWithETag(w, http.StatusCreated, ns)
		return
	}

	// Pre-DB component validation (openapi parse, refs) → 422 before any row.
	if err := validateSpecComponents(req.Spec); err != nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", err.Error())
		return
	}
	d.runCreateSpec(ctx, w, r, p, req)
}

func (d Deps) listNamespaces(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	q := d.DB.WithContext(ctx).
		Where("tenant_id = ? AND status <> ?", p.TenantID, "DELETED")
	if !p.IsAdmin() {
		// Owner-only default; admin sees across owners within the tenant.
		q = q.Where("owner_user_id = ?", p.UserID)
	}
	if s := r.URL.Query().Get("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	var rows []store.Namespace
	if err := q.Order("created_at DESC").Limit(100).Find(&rows).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "list namespaces failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"namespaces": rows, "count": len(rows)})
}

func (d Deps) getNamespace(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	writeWithETag(w, http.StatusOK, ns)
}

type updateRequest struct {
	Description *string `json:"description"`
}

func (d Deps) updateNamespace(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	if !checkIfMatch(w, r, ns.Version) {
		return
	}
	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Description == nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "description is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Optimistic concurrency: WHERE version = current; GORM bumps version.
	res := d.DB.WithContext(ctx).
		Model(&store.Namespace{}).
		Where("id = ? AND version = ?", ns.ID, ns.Version).
		Updates(map[string]any{"description": *req.Description, "version": ns.Version + 1})
	if res.Error != nil || res.RowsAffected == 0 {
		httpx.WriteCodeError(w, "INVALID_TRANSITION", "concurrent modification, retry with fresh If-Match")
		return
	}
	d.audit(ctx, r, p, "namespace.update", ns.ID)
	ns.Description = *req.Description
	ns.Version++
	writeWithETag(w, http.StatusOK, ns)
}

func (d Deps) deleteNamespace(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	actor := store.ActorJSON(p.TenantID, p.UserID, p.Role)
	if err := store.Transition(ctx, d.DB, "namespace", ns.ID, "DELETING", actor, "delete requested"); err != nil {
		// INVALID_TRANSITION from the DB function (e.g. APPLYING/DATA_LOADING).
		httpx.WriteCodeError(w, "INVALID_TRANSITION", "namespace cannot be deleted from its current status")
		return
	}
	d.audit(ctx, r, p, "namespace.delete", ns.ID)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"id": ns.ID, "status": "DELETING"})
}

func (d Deps) events(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var events []store.NamespaceEvent
	if err := d.DB.WithContext(ctx).
		Where("object_id = ?", ns.ID).
		Order("id").Find(&events).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "events query failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": events})
}

// loadOwned fetches the namespace and enforces tenant isolation + owner/admin.
// 404 (not 403) for cross-owner to avoid leaking existence (docs §2.3).
func (d Deps) loadOwned(w http.ResponseWriter, r *http.Request, p authz.Principal) (store.Namespace, bool) {
	id := chi.URLParam(r, "id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	ns, err := store.LoadNamespaceScoped(ctx, d.DB, p.TenantID, p.UserID, p.IsAdmin(), id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "namespace not found")
		return store.Namespace{}, false
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "namespace lookup failed")
		return store.Namespace{}, false
	}
	return ns, true
}

// --- ETag / If-Match -------------------------------------------------------------

func etagFor(version int) string {
	return `"` + strconv.Itoa(version) + `"`
}

func writeWithETag(w http.ResponseWriter, status int, ns store.Namespace) {
	w.Header().Set("ETag", etagFor(ns.Version))
	httpx.WriteJSON(w, status, ns)
}

// checkIfMatch enforces optimistic concurrency; returns false when it wrote a response.
func checkIfMatch(w http.ResponseWriter, r *http.Request, version int) bool {
	im := r.Header.Get("If-Match")
	if im == "" {
		return true // optional; PATCH without If-Match takes latest
	}
	if trimEtag(im) != etagFor(version) {
		w.WriteHeader(http.StatusPreconditionFailed)
		httpx.WriteError(w, http.StatusPreconditionFailed, "PRECONDITION_FAILED",
			"If-Match does not match current version")
		return false
	}
	return true
}

func trimEtag(s string) string { return strings.TrimPrefix(s, "W/") }

// --- helpers ---------------------------------------------------------------------

func (d Deps) audit(ctx context.Context, _ *http.Request, p authz.Principal, action, objectID string) {
	_ = d.DB.WithContext(ctx).Create(&store.AuditLog{
		TenantID:   store.StrPtr(p.TenantID),
		Actor:      []byte(store.ActorJSON(p.TenantID, p.UserID, p.Role)),
		Action:     action,
		ObjectType: "namespace",
		ObjectID:   store.StrPtr(objectID),
		Details:    store.JSONBytes(map[string]any{}),
	}).Error
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
