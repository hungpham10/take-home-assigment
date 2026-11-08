// Package script implements /namespaces/{id}/scripts — k6 script upload with
// the PENDING_VALIDATION → VALIDATING → VALIDATED/INVALID lifecycle and async
// validation (k6 inspect when available, structural checks as fallback).
package script

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/idempotency"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// MaxSize mirrors the ConfigMap limit (1 MiB) enforced by the DB CHECK too.
const MaxSize = 1 << 20

// Deps carries the handlers' dependencies.
type Deps struct {
	DB         *gorm.DB
	ScriptsDir string // staging dir for script content (content_ref)
	Queue      *Queue // validation worker queue (nil in tests → sync validate)
	Now        func() time.Time
}

// Mount registers the script routes FLAT on the passed-in router (server
// composition root applies authz.RequirePrincipal). All patterns carry the
// /namespaces/{id}/scripts prefix.
func Mount(r chi.Router, d Deps) {
	prefix := "/namespaces/{id}/scripts"
	r.With(idempotency.RequireKey).
		Post(prefix, idempotency.Replay("/api/loadtest/v1/scripts", d.DB, d.upload))
	r.Get(prefix, d.list)
	r.Get(prefix+"/{scriptID}", d.get)
	r.Post(prefix+"/{scriptID}/validate", d.revalidate)
	r.Get(prefix+"/{scriptID}/validations", d.validations)
}

// --- handlers --------------------------------------------------------------------

type uploadRequest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

func (d Deps) upload(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := loadNamespace(w, r, p, d.DB)
	if !ok {
		return
	}
	var req uploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Content == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name and content are required")
		return
	}
	if len(req.Content) > MaxSize {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED",
			fmt.Sprintf("script exceeds %d bytes (ConfigMap limit)", MaxSize))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	sum := sha256.Sum256([]byte(req.Content))
	ref, err := d.stage(ns.ID, req.Name, req.Content)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "script staging failed")
		return
	}
	sc := store.Script{
		NamespaceID: ns.ID,
		Name:        req.Name,
		ContentRef:  ref,
		ContentHash: hex.EncodeToString(sum[:]),
		SizeBytes:   len(req.Content),
		Status:      "PENDING_VALIDATION",
		Version:     1,
	}
	if err := d.DB.WithContext(ctx).Create(&sc).Error; err != nil {
		if isUniqueViolation(err) {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "script name already exists")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "script insert failed")
		return
	}
	d.audit(ctx, r, p, "script.create", sc.ID)
	if !d.startValidation(ctx, w, p, sc.ID, "upload") {
		return
	}
	// Reload: sync validation (nil queue) may have already reached a terminal
	// status; respond with the current DB state, not the stale struct.
	_ = d.DB.WithContext(ctx).First(&sc, "id = ?", sc.ID).Error
	httpx.WriteJSON(w, http.StatusCreated, sc)
}

// stage writes content to the staging dir; ref = "<namespaceID>/<name>.js".
func (d Deps) stage(namespaceID, name, content string) (string, error) {
	ref := filepath.Join(namespaceID, sanitize(name)+".js")
	full := filepath.Join(d.ScriptsDir, ref)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return "", err
	}
	return ref, os.WriteFile(full, []byte(content), 0o600) //nolint:gosec // content is user data staged for k6
}

// ReadStaged reads staged content; ref must stay inside scriptsDir
// (path-traversal safe).
func ReadStaged(scriptsDir, ref string) ([]byte, error) {
	clean := filepath.Clean(ref)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return nil, errors.New("script: bad ref")
	}
	return os.ReadFile(filepath.Join(scriptsDir, clean))
}

func (d Deps) list(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := loadNamespace(w, r, p, d.DB)
	if !ok {
		return
	}
	var scripts []store.Script
	if err := d.DB.Where("namespace_id = ?", ns.ID).Order("created_at DESC").Find(&scripts).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "list scripts failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"scripts": scripts, "count": len(scripts)})
}

// loadScript fetches a script within an owned namespace (404-safe).
func (d Deps) loadScript(w http.ResponseWriter, r *http.Request, p authz.Principal) (*store.Script, bool) {
	ns, ok := loadNamespace(w, r, p, d.DB)
	if !ok {
		return nil, false
	}
	var sc store.Script
	if err := d.DB.Where("namespace_id = ? AND id = ?", ns.ID, chi.URLParam(r, "scriptID")).
		First(&sc).Error; err != nil {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "script not found")
		return nil, false
	}
	return &sc, true
}

func (d Deps) get(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	sc, ok := d.loadScript(w, r, p)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sc)
}

// revalidate pushes a script back through validation (INVALID → VALIDATING…).
func (d Deps) revalidate(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	sc, ok := d.loadScript(w, r, p)
	if !ok {
		return
	}
	switch sc.Status {
	case "VALIDATING":
		httpx.WriteCodeError(w, "INVALID_TRANSITION", "script is already validating")
		return
	case "PENDING_VALIDATION", "VALIDATED", "INVALID", "VALIDATION_FAILED":
	default:
		httpx.WriteCodeError(w, "INVALID_TRANSITION", "script cannot be revalidated")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if !d.startValidation(ctx, w, p, sc.ID, "revalidate requested") {
		return
	}
	d.audit(ctx, r, p, "script.revalidate", sc.ID)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"id": sc.ID, "status": "VALIDATING"})
}

func (d Deps) validations(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	sc, ok := d.loadScript(w, r, p)
	if !ok {
		return
	}
	var rows []store.ScriptValidation
	if err := d.DB.Where("script_id = ?", sc.ID).Order("attempt DESC").Find(&rows).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "validations query failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"validations": rows})
}

// --- helpers ---------------------------------------------------------------------

// loadNamespace fetches the namespace from the URL param, enforcing tenant
// isolation + owner/admin via the shared store lookup (404 — no existence leak).
func loadNamespace(w http.ResponseWriter, r *http.Request, p authz.Principal, db *gorm.DB) (store.Namespace, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ns, err := store.LoadNamespaceScoped(ctx, db, p.TenantID, p.UserID, p.IsAdmin(), chi.URLParam(r, "id"))
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

// startValidation moves a script into validation. With a queue: Submit FIRST —
// when the queue is full the script is NOT transitioned (stays re-triggerable)
// and the caller gets 429; on accept the handler performs →VALIDATING (the
// worker only records + transitions to terminal). With a nil queue (unit
// tests) validation runs synchronously so tests observe the final status.
// Returns false when the response has already been written.
func (d Deps) startValidation(ctx context.Context, w http.ResponseWriter, p authz.Principal, scriptID, reason string) bool {
	if d.Queue != nil {
		if !d.Queue.Submit(scriptID) {
			httpx.WriteCodeError(w, "QUOTA_EXCEEDED", "validation queue full, retry")
			return false
		}
	}
	actor := store.ActorJSON(p.TenantID, p.UserID, p.Role)
	if err := store.SCRToValidating(ctx, d.DB, scriptID, actor, reason); err != nil {
		httpx.WriteCodeError(w, "INVALID_TRANSITION", "transition rejected by state machine")
		return false
	}
	if d.Queue == nil {
		_ = ValidateNow(ctx, d.DB, d.ScriptsDir, scriptID)
	}
	return true
}

func (d Deps) audit(ctx context.Context, _ *http.Request, p authz.Principal, action, objectID string) {
	_ = d.DB.WithContext(ctx).Create(&store.AuditLog{
		TenantID:   store.StrPtr(p.TenantID),
		Actor:      []byte(store.ActorJSON(p.TenantID, p.UserID, p.Role)),
		Action:     action,
		ObjectType: "script",
		ObjectID:   store.StrPtr(objectID),
		Details:    store.JSONBytes(map[string]any{}),
	}).Error
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

func sanitize(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
	if name == "" {
		name = "script"
	}
	return name
}
