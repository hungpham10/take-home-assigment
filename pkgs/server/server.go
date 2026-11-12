// Package server wires the loadtest-controller HTTP server: health, ready,
// metrics and the versioned API routers.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/adminapi"
	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/loadtest"
	"github.com/hungpham/loadtest-platform/pkgs/namespace"
	"github.com/hungpham/loadtest-platform/pkgs/runner"
	"github.com/hungpham/loadtest-platform/pkgs/script"
	"github.com/hungpham/loadtest-platform/pkgs/secrets"
	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/version"
)

// Deps carries everything the server routes need.
type Deps struct {
	DB         *gorm.DB
	Now        func() time.Time          // injectable clock (tests)
	Secrets    secrets.Resolver          // SOPS-decrypted tenant secrets
	MasterKey  []byte                    // AES-256-GCM master key (token encryption at rest)
	ScriptsDir string                    // staging dir for k6 scripts
	ScriptQ    *script.Queue             // async validation queue (nil → sync in tests)
	Runner     runner.Runner             // load-test execution backend (local/k8s)
	K8s        k8s.Client                // cluster client for run-scoped NetworkPolicy (nil in legacy/local mode)
	K8S        bool                      // cluster wired → async apply (plans/21); false = legacy sync
	StaleAfter time.Duration             // watcher heartbeat staleness threshold (WATCH-05)
	Net        envreconciler.PlatformNet // platform networking defaults (k6-operator ns, node CIDRs, mock-service)
	// ApplyNow (plans/26): hierarchical apply ngay trong request path sau khi
	// desired-state commit (nil ở legacy mode).
	ApplyNow func(ctx context.Context, namespaceID string)
	// Limits caps vusers/duration/timeout at load-test create (plans/42).
	// Zero value = documented defaults.
	Limits loadtest.Limits
}

// NewRouter builds the full HTTP handler.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version.Get().Version})
	})

	// /version is the runtime truth for "which build is this pod running?"
	// (docs/versioning.md). A deployed image may be older than the manifest
	// tag; this endpoint is what an operator checks.
	r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, version.Get())
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
	// Trust boundary: mounted for the nginx→backend channel; principal
	// middleware applies only to the operator group (nginx internal calls
	// carry no user identity).
	r.Route("/api/admin/v1", func(r chi.Router) {
		adminapi.MountInternal(r, adminapi.Deps{
			DB: d.DB, Now: d.Now, Secrets: d.Secrets, MasterKey: d.MasterKey,
		})
		r.Group(func(r chi.Router) {
			r.Use(authz.RequirePrincipalWithRole(d.DB))
			adminapi.MountOperator(r, adminapi.Deps{
				DB: d.DB, Now: d.Now, Secrets: d.Secrets, MasterKey: d.MasterKey,
			})
		})
	})

	// /api/loadtest/v1 — data-plane BFF (plans 10-13), fail-closed principal
	// with server-side role resolution (X-User-Role is not authoritative).
	r.Route("/api/loadtest/v1", func(r chi.Router) {
		r.Use(authz.RequirePrincipalWithRole(d.DB))
		namespace.Mount(r, namespace.Deps{DB: d.DB, Now: d.Now, K8S: d.K8S, ApplyNow: d.ApplyNow})
		namespace.MountV1(r, namespace.Deps{DB: d.DB, Now: d.Now, K8S: d.K8S, ApplyNow: d.ApplyNow})
		script.Mount(r, script.Deps{DB: d.DB, ScriptsDir: d.ScriptsDir, Queue: d.ScriptQ, Now: d.Now})
		loadtest.Mount(r, loadtest.Deps{DB: d.DB, Runner: d.Runner, Now: d.Now, K8s: d.K8s, K8S: d.K8S, StaleAfter: d.StaleAfter, Net: d.Net, Limits: d.Limits})
	})
	// Script content reader for the runner (staged files).
	loadtest.ScriptContentOf = func(ref string) ([]byte, error) {
		return script.ReadStaged(d.ScriptsDir, ref)
	}
	return r
}
