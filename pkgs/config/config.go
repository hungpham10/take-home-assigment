// Package config loads controller runtime configuration from environment.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config is the loadtest-controller runtime configuration.
type Config struct {
	// HTTP server
	Addr string // :8080

	// PostgreSQL DSN (sql/ migrations are the schema source of truth)
	DatabaseDSN string

	// Runner mode: "local" (default, dev) or "k8s"
	RunnerMode string

	// Log level: debug|info|warn|error
	LogLevel string

	// ScriptsDir stages k6 script content (content_ref in tbl_scripts)
	ScriptsDir string

	// K8S enables the cluster path (plans/21): Env Reconciler + Status
	// Watcher run in-process and POST /apply becomes async. Requires an
	// API server URL (K8S_API_URL, default in-cluster service env).
	K8SEnabled bool
	K8SAPIURL  string

	// Status Watcher timing (docs/namespace-status-watcher.md §8; seconds).
	WatcherResyncSeconds      int
	WatcherGraceSeconds       int
	WatcherStableSeconds      int
	WatcherApplyDeadlineMin   int
	WatcherStalenessThreshold int

	// HA enables K8s leader election for the control loops (docs/k8s-orchestration.md §10).
	// Only effective when K8SEnabled=true. Default false (single-pod).
	HAEnabled      bool
	HAIdentity     string // unique per pod (default hostname / pod name)
	HALeaseName    string // name of coordination Lease resource
	HALeaseNamespace string // namespace holding the Lease
}

// FromEnv builds Config from environment variables with sane dev defaults.
func FromEnv() (Config, error) {
	c := Config{
		Addr:        envOr("ADDR", ":8080"),
		DatabaseDSN: envOr("DATABASE_DSN", "postgres://ltp:ltp@localhost:5432/ltp?sslmode=disable"),
		RunnerMode:  envOr("RUNNER_MODE", "local"),
		LogLevel:    envOr("LOG_LEVEL", "info"),
		ScriptsDir:  envOr("SCRIPTS_DIR", "/tmp/ltp-scripts"),
		K8SEnabled:  envOr("K8S_ENABLED", "false") == "true",
		K8SAPIURL:   envOr("K8S_API_URL", "https://kubernetes.default.svc"),
		HAEnabled:   envOr("HA_ENABLED", "false") == "true",
		HAIdentity:  envOr("HA_IDENTITY", ""),
		HALeaseName:     envOr("HA_LEASE_NAME", "loadtest-controller-leader"),
		HALeaseNamespace: envOr("HA_LEASE_NAMESPACE", ""),
	}
	var err error
	if c.WatcherResyncSeconds, err = envInt("WATCHER_RESYNC_PERIOD", 300); err != nil {
		return Config{}, err
	}
	if c.WatcherGraceSeconds, err = envInt("WATCHER_GRACE_WINDOW", 60); err != nil {
		return Config{}, err
	}
	if c.WatcherStableSeconds, err = envInt("WATCHER_STABLE_WINDOW", 60); err != nil {
		return Config{}, err
	}
	if c.WatcherApplyDeadlineMin, err = envInt("WATCHER_APPLY_DEADLINE", 15); err != nil {
		return Config{}, err
	}
	if c.WatcherStalenessThreshold, err = envInt("WATCHER_STALENESS_THRESHOLD", 900); err != nil {
		return Config{}, err
	}
	switch c.RunnerMode {
	case "local", "k8s":
	default:
		return Config{}, fmt.Errorf("invalid RUNNER_MODE %q: must be local or k8s", c.RunnerMode)
	}
	if port, err := strconv.Atoi(envOr("PORT", "")); err == nil && port > 0 {
		c.Addr = fmt.Sprintf(":%d", port)
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s %q: must be a positive integer", key, v)
	}
	return n, nil
}
