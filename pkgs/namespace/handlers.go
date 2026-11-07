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
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
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
	return r
}

// --- models --------------------------------------------------------------------

type namespaceRow struct {
	ID               string     `gorm:"column:id"`
	TenantID         string     `gorm:"column:tenant_id"`
	OwnerUserID      string     `gorm:"column:owner_user_id"`
	Name             string     `gorm:"column:name"`
	Description      string     `gorm:"column:description"`
	Status           string     `gorm:"column:status"`
	ReadyForLoadtest bool       `gorm:"column:ready_for_loadtest"`
	Version          int        `gorm:"column:version"`
	TTLExpiresAt     *time.Time `gorm:"column:ttl_expires_at"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

type createRequest struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	TTL         *time.Time `json:"ttl_expires_at"`
}

type updateRequest struct {
	Description *string `json:"description"`
	// Optimistic concurrency via If-Match header.
}

// --- handlers --------------------------------------------------------------------

func (d Deps) create(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var row namespaceRow
	err := d.DB.WithContext(ctx).Raw(`
		INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name, description, ttl_expires_at)
		VALUES (?::uuid, ?::uuid, ?, ?, ?)
		RETURNING id::text, tenant_id::text, owner_user_id::text, name, description,
		          status, ready_for_loadtest, version, ttl_expires_at, created_at, updated_at`,
		p.TenantID, p.UserID, req.Name, req.Description, req.TTL).
		Scan(&row).Error
	if err != nil {
		if isUniqueViolation(err) {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "namespace name already exists in tenant")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "create namespace failed")
		return
	}
	d.audit(ctx, r, p, "namespace.create", row.ID)
	writeWithETag(w, http.StatusCreated, row)
}

func (d Deps) list(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	q := `SELECT id::text, tenant_id::text, owner_user_id::text, name, description,
	             status, ready_for_loadtest, version, ttl_expires_at, created_at, updated_at
	      FROM tbl_namespaces WHERE tenant_id = ?::uuid AND status <> 'DELETED'`
	args := []any{p.TenantID}
	if !p.IsAdmin() {
		// Owner-only default; admin sees across owners within the tenant.
		q += ` AND owner_user_id = ?::uuid`
		args = append(args, p.UserID)
	}
	if s := r.URL.Query().Get("status"); s != "" {
		q += ` AND status = ?`
		args = append(args, s)
	}
	q += ` ORDER BY created_at DESC LIMIT 100`

	var rows []namespaceRow
	if err := d.DB.WithContext(ctx).Raw(q, args...).Scan(&rows).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "list namespaces failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"namespaces": rows, "count": len(rows)})
}

func (d Deps) get(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	writeWithETag(w, http.StatusOK, row)
}

func (d Deps) update(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	if !checkIfMatch(w, r, row.Version) {
		return
	}
	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Description == nil {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "description is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var updated namespaceRow
	err := d.DB.WithContext(ctx).Raw(`
		UPDATE tbl_namespaces SET description = ?, version = version + 1
		WHERE id = ?::uuid AND version = ?
		RETURNING id::text, tenant_id::text, owner_user_id::text, name, description,
		          status, ready_for_loadtest, version, ttl_expires_at, created_at, updated_at`,
		*req.Description, row.ID, row.Version).Scan(&updated).Error
	if err != nil || updated.ID == "" {
		// version changed under us
		httpx.WriteCodeError(w, "INVALID_TRANSITION", "concurrent modification, retry with fresh If-Match")
		return
	}
	d.audit(ctx, r, p, "namespace.update", row.ID)
	writeWithETag(w, http.StatusOK, updated)
}

func (d Deps) deleteNamespace(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var tRow struct {
		Status  string
		Version int
	}
	err := d.DB.WithContext(ctx).Raw(`
		SELECT status, version FROM fn_namespace_transition(?::uuid, 'DELETING', ?::jsonb, ?)`,
		row.ID, authzJSON(p), "delete requested").Scan(&tRow).Error
	if err != nil {
		// INVALID_TRANSITION from the DB function (e.g. APPLYING/DATA_LOADING).
		httpx.WriteCodeError(w, "INVALID_TRANSITION", "namespace cannot be deleted from its current status")
		return
	}
	d.audit(ctx, r, p, "namespace.delete", row.ID)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"id": row.ID, "status": tRow.Status})
}

func (d Deps) events(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	row, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	type eventRow struct {
		ID         int64     `gorm:"column:id"`
		FromStatus *string   `gorm:"column:from_status"`
		ToStatus   string    `gorm:"column:to_status"`
		Reason     *string   `gorm:"column:reason"`
		CreatedAt  time.Time `gorm:"column:created_at"`
	}
	var events []eventRow
	if err := d.DB.WithContext(ctx).Raw(`
		SELECT id, from_status, to_status, reason, created_at
		FROM tbl_namespace_events WHERE object_id = ?::uuid ORDER BY id`, row.ID).
		Scan(&events).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "events query failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": events})
}

// loadOwned fetches the namespace and enforces tenant isolation + owner/admin.
// 404 (not 403) for cross-owner to avoid leaking existence (docs §2.3).
func (d Deps) loadOwned(w http.ResponseWriter, r *http.Request, p authz.Principal) (namespaceRow, bool) {
	id := chi.URLParam(r, "id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var row namespaceRow
	q := `SELECT id::text, tenant_id::text, owner_user_id::text, name, description,
	             status, ready_for_loadtest, version, ttl_expires_at, created_at, updated_at
	      FROM tbl_namespaces WHERE id = ?::uuid AND tenant_id = ?::uuid AND status <> 'DELETED'`
	args := []any{id, p.TenantID}
	if !p.IsAdmin() {
		q += ` AND owner_user_id = ?::uuid`
		args = append(args, p.UserID)
	}
	err := d.DB.WithContext(ctx).Raw(q, args...).Scan(&row).Error
	if err != nil || row.ID == "" {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "namespace not found")
		return namespaceRow{}, false
	}
	return row, true
}

// --- ETag / If-Match -------------------------------------------------------------

func etagFor(version int) string {
	return `"` + strconv.Itoa(version) + `"`
}

func writeWithETag(w http.ResponseWriter, status int, row namespaceRow) {
	w.Header().Set("ETag", etagFor(row.Version))
	httpx.WriteJSON(w, status, row)
}

// checkIfMatch enforces optimistic concurrency; returns false when it wrote a response.
func checkIfMatch(w http.ResponseWriter, r *http.Request, version int) bool {
	im := r.Header.Get("If-Match")
	if im == "" {
		return true // optional; PATCH without If-Match takes latest
	}
	if strings.Trim(im, ` W/`) != etagFor(version) && strings.Trim(im, `"`) != etagFor(version) {
		w.WriteHeader(http.StatusPreconditionFailed)
		httpx.WriteError(w, http.StatusPreconditionFailed, "PRECONDITION_FAILED",
			"If-Match does not match current version")
		return false
	}
	return true
}

// --- idempotency --------------------------------------------------------------------

// requireIdempotencyKey enforces the mandatory Idempotency-Key header on
// POST /namespaces, /apply, /loadtests, /loadtests/{id}/stop (docs contract).
func requireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "Idempotency-Key header is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// idempotencyReplay replays a stored response for (tenant, endpoint, key)
// with identical request hash. Returns handled=true when replayed or conflict.
// Mount via With(...) around create-like handlers.
func (d Deps) idempotencyReplay(endpoint string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := authz.PrincipalOf(r)
		key := r.Header.Get("Idempotency-Key")
		body, err := readBody(r)
		if err != nil {
			httpx.WriteCodeError(w, "BAD_REQUEST", "unreadable request body")
			return
		}
		sum := sha256.Sum256(body)
		reqHash := hex.EncodeToString(sum[:])

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var stored struct {
			RequestHash    string  `gorm:"column:request_hash"`
			ResponseStatus *int    `gorm:"column:response_status"`
			ResponseBody   *string `gorm:"column:response_body"`
		}
		err = d.DB.WithContext(ctx).Raw(`
			SELECT request_hash, response_status, response_body
			FROM tbl_idempotency_keys
			WHERE tenant_id = ?::uuid AND endpoint = ? AND key = ?`,
			p.TenantID, endpoint, key).Scan(&stored).Error
		if err == nil && stored.RequestHash != "" {
			if stored.RequestHash != reqHash {
				httpx.WriteCodeError(w, "VALIDATION_FAILED",
					"Idempotency-Key reused with different request payload")
				return
			}
			if stored.ResponseStatus != nil && stored.ResponseBody != nil {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotency-Replayed", "true")
				w.WriteHeader(*stored.ResponseStatus)
				_, _ = w.Write([]byte(*stored.ResponseBody))
				return
			}
			// In-flight duplicate: first request still processing.
			httpx.WriteCodeError(w, "INVALID_TRANSITION", "request with this Idempotency-Key is in progress")
			return
		}

		rec := &captureWriter{ResponseWriter: w}
		r.Body = readCloser(body)
		next(rec, r)

		if rec.status >= 200 && rec.status < 500 {
			_ = d.DB.WithContext(ctx).Exec(`
				INSERT INTO tbl_idempotency_keys (tenant_id, endpoint, key, request_hash, response_status, response_body)
				VALUES (?::uuid, ?, ?, ?, ?, NULLIF(?,'')::jsonb)
				ON CONFLICT (tenant_id, endpoint, key) DO NOTHING`,
				p.TenantID, endpoint, key, reqHash, rec.status, rec.bodyString()).Error
		}
	}
}

// --- helpers ---------------------------------------------------------------------

func (d Deps) audit(ctx context.Context, _ *http.Request, p authz.Principal, action, objectID string) {
	_ = d.DB.WithContext(ctx).Exec(`
		INSERT INTO tbl_audit_log (tenant_id, actor, action, object_type, object_id)
		VALUES (?::uuid, ?::jsonb, ?, 'namespace', ?)`,
		p.TenantID, actorJSON(p), action, objectID).Error
}

func authzJSON(p authz.Principal) string {
	b, _ := json.Marshal(map[string]string{"tenant_id": p.TenantID, "user_id": p.UserID, "role": p.Role})
	return string(b)
}

func actorJSON(p authz.Principal) string {
	return authzJSON(p)
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
