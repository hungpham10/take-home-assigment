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
	"github.com/hungpham/loadtest-platform/pkgs/listener"
	"github.com/hungpham/loadtest-platform/pkgs/loadtest"
	"github.com/hungpham/loadtest-platform/pkgs/script"
	"github.com/hungpham/loadtest-platform/pkgs/secrets"
	"github.com/hungpham/loadtest-platform/pkgs/server"
	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// `--version` must work before any config/env/DB is touched: it is the
	// "which build is this?" probe used in images and CI (docs/versioning.md).
	if version.PrintIfRequested(os.Args[1:], os.Stdout) {
		return nil
	}

	// CLI subcommand: sops encrypt/decrypt (chỉ cần LTP_MASTER_KEY, không cần
	// DB). Thực thi trước config.FromEnv() để không bị choke khi
	// DATABASE_DSN chưa set.
	if len(os.Args) > 1 && os.Args[1] == "sops" {
		sopsCmd := secrets.SopsSubcommand{}
		if err := sopsCmd.Run(os.Args[2:]); err != nil {
			return err
		}
		return nil
	}

	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)
	slog.Info("starting", "version", version.Get().Version, "commit", version.Get().Commit, "build_date", version.Get().BuildDate)
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

	// k8s client is required for the k8s runner (RUNNER_MODE=k8s) and for
	// run-scoped NetworkPolicy / HA. Build it before picking the runner so
	// k8s mode is fail-closed (no silent local fallback in a cluster).
	var k8sClient k8s.Client
	if cfg.K8SEnabled {
		var err error
		k8sClient, err = k8s.NewClientFromEnv(cfg.K8SAPIURL)
		if err != nil {
			return fmt.Errorf("controller: build k8s client: %w", err)
		}
	}

	run, err := newRunner(cfg.RunnerMode, "", k8sClient)
	if err != nil {
		return err
	}
	// Reconciler: poll Runner status → advance DB state machine.
	go loadtest.Reconciler{DB: db, Runner: run, K8s: k8sClient, Every: 2 * time.Second}.Start(ctx)

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

	net := platformNet(cfg)

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: server.NewRouter(server.Deps{DB: db, Secrets: secrets.New(), MasterKey: secrets.LoadMasterKey(), ScriptsDir: cfg.ScriptsDir, ScriptQ: scriptQ, Runner: run, K8s: k8sClient, K8S: cfg.K8SEnabled, StaleAfter: time.Duration(cfg.WatcherStalenessThreshold) * time.Second,
			Net:      net,
			Limits:   loadtest.Limits{MaxVUs: cfg.MaxVUs, MaxDuration: cfg.MaxDuration, MaxTimeoutMinutes: cfg.MaxTimeoutMinutes},
			ApplyNow: applyNowFn(db, cfg.K8SAPIURL, func() bool { return leaderGate == nil || leaderGate.IsLeader() }, net)}),
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
	// listener.New supports ADDR="unix:/var/run/axum" (nginx upstream, plans/34)
	// as well as plain TCP addresses.
	ln, err := listener.New(cfg.Addr)
	if err != nil {
		return err
	}
	go func() { errCh <- srv.Serve(ln) }()
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
