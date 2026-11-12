// cmds/mock-service — the data-plane mock runtime (plans/21 PR D,
// docs/mock-service-api.md). A SEPARATE deployment unit from the controller:
// no PostgreSQL, no authz, no public ingress (cluster-internal only). Desired
// state arrives as a versioned snapshot file (binding ConfigMap mounted by
// kind, or a local path in dev); the runtime serves mock routes with latency
// injection and reports observed_generation for the watcher's mock evaluator.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/mockruntime"
	"github.com/hungpham/loadtest-platform/pkgs/version"
)

func main() {
	if version.PrintIfRequested(os.Args[1:], os.Stdout) {
		return
	}

	addr := envOr("ADDR", ":8090")
	snapshotPath := envOr("MOCK_SNAPSHOT_FILE", "") // optional initial snapshot

	rt := mockruntime.New()
	if snapshotPath != "" {
		if err := mockruntime.LoadFile(rt, snapshotPath); err != nil {
			slog.Warn("initial snapshot not loaded", "path", snapshotPath, "err", err)
		}
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           rt.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	slog.Info("mock-service listening", "addr", addr, "generation", rt.ObservedGeneration(), "version", version.Get().Version, "commit", version.Get().Commit)
	select {
	case sig := <-sigCh:
		slog.Info("shutting down", "signal", sig)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
