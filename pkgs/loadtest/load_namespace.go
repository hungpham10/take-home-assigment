package loadtest

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// loadNamespace fetches an owned namespace (404-safe, shared by all handlers).
func loadNamespace(w http.ResponseWriter, r *http.Request, p authz.Principal, db *gorm.DB) (store.Namespace, bool) {
	id := chi.URLParam(r, "id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ns, err := store.LoadNamespaceScoped(ctx, db, p.TenantID, p.UserID, p.IsAdmin(), id)
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

// ScriptReader reads staged script content; the server wiring injects the
// scripts-dir-backed implementation (avoids importing pkgs/script here).
type ScriptReader func(contentRef string) ([]byte, error)

// ScriptContentOf reads content via the package-level reader (set at wiring).
var ScriptContentOf ScriptReader = func(string) ([]byte, error) { return nil, nil }

func scriptContent(_ context.Context, _ *gorm.DB, sc store.Script) []byte {
	b, _ := ScriptContentOf(sc.ContentRef)
	return b
}
