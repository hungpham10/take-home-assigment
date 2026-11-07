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
}

// FromEnv builds Config from environment variables with sane dev defaults.
func FromEnv() (Config, error) {
	c := Config{
		Addr:        envOr("ADDR", ":8080"),
		DatabaseDSN: envOr("DATABASE_DSN", "postgres://ltp:ltp@localhost:5432/ltp?sslmode=disable"),
		RunnerMode:  envOr("RUNNER_MODE", "local"),
		LogLevel:    envOr("LOG_LEVEL", "info"),
		ScriptsDir:  envOr("SCRIPTS_DIR", "/tmp/ltp-scripts"),
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
