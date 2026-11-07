// Command loadtest-controller is the control plane BFF for the load testing
// platform: namespace/component CRUD, OpenAPI validation, script/loadtest
// lifecycle and RBAC.
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

	platform "github.com/hungpham/loadtest-platform"
	"github.com/hungpham/loadtest-platform/pkgs/config"
	"github.com/hungpham/loadtest-platform/pkgs/script"
	"github.com/hungpham/loadtest-platform/pkgs/secrets"
	"github.com/hungpham/loadtest-platform/pkgs/server"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Migrations first — sql/ is the schema source of truth.
	if err := store.Migrate(ctx, cfg.DatabaseDSN, platform.Migrations()); err != nil {
		return err
	}
	slog.Info("migrations applied")

	// Async script validation workers (k6 inspect local; K8s Job in plan 13).
	scriptQ := script.NewQueue(ctx, nil, cfg.ScriptsDir, 4, 256) // db wired below
	_ = scriptQ

	db, err := store.New(ctx, cfg.DatabaseDSN)
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()

	// Re-wire the queue with the live DB handle (workers started before db).
	*scriptQ = *script.NewQueue(ctx, db, cfg.ScriptsDir, 4, 256)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.NewRouter(server.Deps{DB: db, Secrets: secrets.New(), MasterKey: secrets.LoadMasterKey(), ScriptsDir: cfg.ScriptsDir, ScriptQ: scriptQ}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("controller listening", "addr", cfg.Addr, "runner", cfg.RunnerMode)

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}

func setupLogging(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})))
}
