// Command loadtest-controller is the control plane BFF for the load testing
// platform: namespace/component CRUD, OpenAPI validation, script/loadtest
// lifecycle and RBAC.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	platform "github.com/hungpham/loadtest-platform"
	"github.com/hungpham/loadtest-platform/pkgs/config"
	"github.com/hungpham/loadtest-platform/pkgs/ha"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/loadtest"
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

	db, err := store.New(ctx, cfg.DatabaseDSN)
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()

	// Async script validation workers (k6 inspect local; K8s Job plugs later).
	scriptQ := script.NewQueue(ctx, db, cfg.ScriptsDir, 4, 256)

	run := newRunner(cfg.RunnerMode, "")
	// Reconciler: poll Runner status → advance DB state machine.
	go loadtest.Reconciler{DB: db, Runner: run, Every: 2 * time.Second}.Start(ctx)

	// HA leader election (docs/k8s-orchestration.md §10): single lease,
	// leader writes, standby no-ops. Created before the router so ApplyNow
	// can gate on it.
	var leaderGate *ha.Gate
	if cfg.K8SEnabled && cfg.HAEnabled {
		kubeClient, err := k8s.NewClientsetFromEnv(cfg.K8SAPIURL)
		if err != nil {
			return fmt.Errorf("controller: build k8s clientset for HA: %w", err)
		}
		identity := cfg.HAIdentity
		if identity == "" {
			if h, err := os.Hostname(); err == nil {
				identity = h
			}
		}
		leaderGate = ha.NewGate(kubeClient, ha.DefaultConfig(identity, cfg.HALeaseName, cfg.HALeaseNamespace))
		go func() {
			if err := leaderGate.Run(ctx); err != nil && err != context.Canceled {
				slog.Warn("controller: leader election stopped", "err", err)
			}
		}()
	}

	var k8sClient k8s.Client
	if cfg.K8SEnabled {
		var err error
		k8sClient, err = k8s.NewClientFromEnv(cfg.K8SAPIURL)
		if err != nil {
			return fmt.Errorf("controller: build k8s client: %w", err)
		}
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: server.NewRouter(server.Deps{DB: db, Secrets: secrets.New(), MasterKey: secrets.LoadMasterKey(), ScriptsDir: cfg.ScriptsDir, ScriptQ: scriptQ, Runner: run, K8s: k8sClient, K8S: cfg.K8SEnabled, StaleAfter: time.Duration(cfg.WatcherStalenessThreshold) * time.Second,
			ApplyNow: applyNowFn(db, cfg.K8SAPIURL, func() bool { return leaderGate == nil || leaderGate.IsLeader() })}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Two control loops when the cluster path is enabled (plans/21).
	if cfg.K8SEnabled {
		if err := startOrchestration(ctx, db, cfg, cfg.K8SAPIURL, func() bool { return leaderGate == nil || leaderGate.IsLeader() },
			time.Duration(cfg.WatcherResyncSeconds)*time.Second,
			time.Duration(cfg.WatcherGraceSeconds)*time.Second,
			time.Duration(cfg.WatcherStableSeconds)*time.Second,
			time.Duration(cfg.WatcherApplyDeadlineMin)*time.Minute,
			time.Duration(cfg.WatcherStalenessThreshold)*time.Second,
		); err != nil {
			slog.Warn("orchestration loops disabled", "err", err)
		}
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
