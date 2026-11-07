// Package server wires the loadtest-controller HTTP server: health, ready,
// metrics and the versioned API routers.
package server

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/adminapi"
	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/namespace"
	"github.com/hungpham/loadtest-platform/pkgs/secrets"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Deps carries everything the server routes need.
type Deps struct {
	DB      *gorm.DB
	Now     func() time.Time // injectable clock (tests)
	Secrets secrets.Resolver // SOPS-decrypted tenant secrets
}

// NewRouter builds the full HTTP handler.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Get("/ready", func(w http.ResponseWriter, req *http.Request) {
		if d.DB == nil || store.Ping(req.Context(), d.DB) != nil {
			httpx.WriteError(w, http.StatusServiceUnavailable, "NOT_READY", "database unavailable")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	r.Method(http.MethodGet, "/metrics", promhttp.Handler())

	// /api/admin/v1 — nginx integration + account management (plan 09).
	// Trust boundary: mounted for the nginx→backend channel; middleware
	// FailClosed applies to /api/loadtest/v1 (plan 10), not here (nginx
	// internal calls carry no user identity).
	r.Mount("/api/admin/v1", adminapi.Router(adminapi.Deps{DB: d.DB, Now: d.Now, Secrets: d.Secrets}))

	// /api/loadtest/v1 — data-plane BFF (plans 10-13), fail-closed principal.
	r.Route("/api/loadtest/v1", func(r chi.Router) {
		r.Use(authz.RequirePrincipal)
		r.Mount("/namespaces", namespace.Router(namespace.Deps{DB: d.DB, Now: d.Now}))
	})
	return r
}
