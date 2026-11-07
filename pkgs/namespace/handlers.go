// Package namespace implements /api/loadtest/v1/namespaces — namespace CRUD
// with Idempotency-Key, ETag optimistic concurrency and DB-driven state
// transitions (fn_namespace_transition; app never UPDATEs status directly).
package namespace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Deps carries the handlers' dependencies.
type Deps struct {
	DB  *gorm.DB
	Now func() time.Time
}

// Router mounts namespace routes (under /api/loadtest/v1).
func Router(d Deps) http.Handler {
	r := chi.NewRouter()
	r.With(requireIdempotencyKey).
		Post("/", d.idempotencyReplay("/api/loadtest/v1/namespaces", d.create))
	r.Get("/", d.list)
	r.Get("/{id}", d.get)
	r.Patch("/{id}", d.update)
	// Soft delete: DRAFT/INVALID/READY/DEGRADED → DELETING (reaper finishes it).
	r.Delete("/{id}", d.deleteNamespace)
	r.Get("/{id}/events", d.events)
	// NOTE: no Mount("/{id}", ...) — a chi subtree at the dynamic param path
	// swallows the exact GET /{id} route.
	r.Mount("/{id}/services", ServicesRouter(d))
	r.Post("/{id}/validate", d.validateNamespace)
	r.With(requireIdempotencyKey).
		Post("/{id}/apply", d.idempotencyReplay("/api/loadtest/v1/apply", d.applyNamespace))
	r.Get("/{id}/readiness", d.readiness)
	return r
}

// --- handlers --------------------------------------------------------------------

type createRequest struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	TTL         *time.Time `json:"ttl_expires_at"`
}

func (d Deps) create(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	ns := store.Namespace{
		TenantID:      p.TenantID,
		OwnerUserID:   p.UserID,
		Name:          req.Name,
		Description:   req.Description,
		Status:        "DRAFT",
		Version:       1,
		TTLExpiresAt:  req.TTL,
		CleanupPolicy: jsonBytes(map[string]any{}),
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
}

func (d Deps) list(w http.ResponseWriter, r *http.Request) {
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

func (d Deps) get(w http.ResponseWriter, r *http.Request) {
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

func (d Deps) update(w http.ResponseWriter, r *http.Request) {
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

	var ns store.Namespace
	q := d.DB.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND status <> ?", id, p.TenantID, "DELETED")
	if !p.IsAdmin() {
		q = q.Where("owner_user_id = ?", p.UserID)
	}
	err := q.First(&ns).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && ns.ID == "") {
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

func trimEtag(s string) string { return trimEtagNormal(s) }

// --- idempotency --------------------------------------------------------------------

// requireIdempotencyKey enforces the mandatory Idempotency-Key header on
// POST /namespaces, /apply, /loadtests, /loadtests/{id}/stop (docs contract).
func requireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "" {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "Idempotency-Key header is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// idempotencyReplay replays a stored response for (tenant, endpoint, key)
// with identical request hash; otherwise calls next and stores the response.
func (d Deps) idempotencyReplay(endpoint string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := authz.PrincipalOf(r)
		key := r.Header.Get("Idempotency-Key")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpx.WriteCodeError(w, "BAD_REQUEST", "unreadable request body")
			return
		}
		sum := sha256.Sum256(body)
		reqHash := hex.EncodeToString(sum[:])

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var stored store.IdempotencyKey
		err = d.DB.WithContext(ctx).
			Where("tenant_id = ? AND endpoint = ? AND key = ?", p.TenantID, endpoint, key).
			First(&stored).Error
		switch {
		case err == nil && stored.RequestHash != reqHash:
			httpx.WriteCodeError(w, "VALIDATION_FAILED",
				"Idempotency-Key reused with different request payload")
			return
		case err == nil && stored.ResponseStatus != nil && stored.ResponseBody != nil:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(*stored.ResponseStatus)
			_, _ = w.Write([]byte(*stored.ResponseBody))
			return
		case err == nil:
			// In-flight duplicate: first request still processing.
			httpx.WriteCodeError(w, "INVALID_TRANSITION", "request with this Idempotency-Key is in progress")
			return
		}

		rec := &captureWriter{ResponseWriter: w}
		r.Body = io.NopCloser(bytesReader(body))
		next(rec, r)

		if rec.status >= 200 && rec.status < 500 {
			_ = d.DB.WithContext(ctx).Create(&store.IdempotencyKey{
				TenantID:       p.TenantID,
				Endpoint:       endpoint,
				Key:            key,
				RequestHash:    reqHash,
				ResponseStatus: &rec.status,
				ResponseBody:   strPtr(rec.bodyString()),
			}).Error
		}
	}
}

// --- helpers ---------------------------------------------------------------------

func (d Deps) audit(ctx context.Context, _ *http.Request, p authz.Principal, action, objectID string) {
	_ = d.DB.WithContext(ctx).Create(&store.AuditLog{
		TenantID:   strPtr(p.TenantID),
		Actor:      jsonBytes(map[string]string{"tenant_id": p.TenantID, "user_id": p.UserID, "role": p.Role}),
		Action:     action,
		ObjectType: "namespace",
		ObjectID:   strPtr(objectID),
		Details:    jsonBytes(map[string]any{}),
	}).Error
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
