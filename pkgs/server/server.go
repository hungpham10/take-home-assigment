// Package server wires the loadtest-controller HTTP server: health, ready,
// metrics and the versioned API routers (mounted in later plans).
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hungpham/loadtest-platform/pkgs/httpx"
)

// Deps carries everything the server routes need.
type Deps struct {
	Pool *pgxpool.Pool
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

	r.Get("/ready", func(w http.ResponseWriter, _ *http.Request) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if d.Pool == nil || d.Pool.Ping(ctx) != nil {
			httpx.WriteError(w, http.StatusServiceUnavailable, "NOT_READY", "database unavailable")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	r.Method(http.MethodGet, "/metrics", promhttp.Handler())

	// /api/admin/v1 and /api/loadtest/v1 mount in plans 09-13.
	return r
}
