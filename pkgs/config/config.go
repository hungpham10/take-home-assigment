// Package config loads controller runtime configuration from environment.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
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

	// Platform networking (docs/network-policy.md Configuration).
	// k6-operator controller namespace: StartJobs probes the runner API from
	// here; cross-namespace ingress is allowed by the run-scoped policy.
	K6OperatorNamespace string
	// Node CIDRs reachable for kube-apiserver (node IP:6443/443) because
	// kube-proxy DNATs the service IP before netpol checks. Production clusters
	// must set this explicitly; default targets kind/colima.
	K8SNodeCIDRs []string
	// mock-service runtime namespace (ExternalName Service target lives here).
	MockServiceNamespace string
	// mock-service ports allowed for egress SUT/runner → mock-service.
	MockServicePorts []int32
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
		K6OperatorNamespace: envOr("K6_OPERATOR_NAMESPACE", "k6-operator-system"),
		MockServiceNamespace: envOr("MOCK_SERVICE_NAMESPACE", "mock-system"),
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
	if c.K8SNodeCIDRs, err = envCIDRs("K8S_NODE_CIDRS", "172.18.0.0/16"); err != nil {
		return Config{}, err
	}
	if c.MockServicePorts, err = envPorts("MOCK_SERVICE_PORTS", "80,8090"); err != nil {
		return Config{}, err
	}
	return c, nil
}

// envCIDRs parses a comma list of CIDRs, dropping empty/whitespace entries.
// Returns the default when the env var is unset. Invalid CIDRs produce an error.
func envCIDRs(key, def string) ([]string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return []string{def}, nil
	}
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(part); err != nil {
			return nil, fmt.Errorf("invalid %s %q: must be a valid CIDR", key, part)
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return []string{def}, nil
	}
	return out, nil
}

// envPorts parses a comma list of TCP ports (1–65535), dropping empty entries.
// Returns the default when the env var is unset.
func envPorts(key, def string) ([]int32, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return envPortsList(def)
	}
	out := []int32{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid %s %q: must be a port 1-65535", key, part)
		}
		out = append(out, int32(n))
	}
	if len(out) == 0 {
		return envPortsList(def)
	}
	return out, nil
}

func envPortsList(def string) ([]int32, error) {
	out := []int32{}
	for _, part := range strings.Split(def, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid default port %q for %q", part, def)
		}
		out = append(out, int32(n))
	}
	return out, nil
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
