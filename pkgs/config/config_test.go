package config

import (
	"strings"
	"testing"
)

func TestFromEnvDefaults(t *testing.T) {
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", c.Addr)
	}
	if c.RunnerMode != "local" {
		t.Errorf("RunnerMode = %q, want local", c.RunnerMode)
	}
	if c.DatabaseDSN == "" {
		t.Error("DatabaseDSN must have a default")
	}
	// Platform networking defaults.
	if c.K6OperatorNamespace != "k6-operator-system" {
		t.Errorf("K6OperatorNamespace = %q, want k6-operator-system", c.K6OperatorNamespace)
	}
	if len(c.K8SNodeCIDRs) != 1 || c.K8SNodeCIDRs[0] != "172.18.0.0/16" {
		t.Errorf("K8SNodeCIDRs = %v, want [172.18.0.0/16]", c.K8SNodeCIDRs)
	}
	if c.MockServiceNamespace != "mock-system" {
		t.Errorf("MockServiceNamespace = %q, want mock-system", c.MockServiceNamespace)
	}
	if len(c.MockServicePorts) != 2 || c.MockServicePorts[0] != 80 || c.MockServicePorts[1] != 8090 {
		t.Errorf("MockServicePorts = %v, want [80 8090]", c.MockServicePorts)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("RUNNER_MODE", "k8s")
	t.Setenv("LOG_LEVEL", "debug")
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.Addr != ":9090" {
		t.Errorf("PORT override: Addr = %q, want :9090", c.Addr)
	}
	if c.RunnerMode != "k8s" {
		t.Errorf("RunnerMode = %q, want k8s", c.RunnerMode)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", c.LogLevel)
	}
}

func TestFromEnvInvalidRunnerMode(t *testing.T) {
	t.Setenv("RUNNER_MODE", "swarm")
	if _, err := FromEnv(); err == nil {
		t.Error("expected error for invalid RUNNER_MODE")
	}
}

func TestPlatformNetworkOverrides(t *testing.T) {
	t.Setenv("K6_OPERATOR_NAMESPACE", "k6-operator-prod")
	t.Setenv("K8S_NODE_CIDRS", "10.0.0.0/8, 192.168.0.0/24 ,")
	t.Setenv("MOCK_SERVICE_NAMESPACE", "mock-prod")
	t.Setenv("MOCK_SERVICE_PORTS", "8080,8090")
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.K6OperatorNamespace != "k6-operator-prod" {
		t.Errorf("K6OperatorNamespace = %q, want k6-operator-prod", c.K6OperatorNamespace)
	}
	// Trailing/empty entries are dropped; whitespace trimmed.
	if len(c.K8SNodeCIDRs) != 2 || c.K8SNodeCIDRs[0] != "10.0.0.0/8" || c.K8SNodeCIDRs[1] != "192.168.0.0/24" {
		t.Errorf("K8SNodeCIDRs = %v, want [10.0.0.0/8 192.168.0.0/24]", c.K8SNodeCIDRs)
	}
	if c.MockServiceNamespace != "mock-prod" {
		t.Errorf("MockServiceNamespace = %q, want mock-prod", c.MockServiceNamespace)
	}
	if len(c.MockServicePorts) != 2 || c.MockServicePorts[0] != 8080 || c.MockServicePorts[1] != 8090 {
		t.Errorf("MockServicePorts = %v, want [8080 8090]", c.MockServicePorts)
	}
}

func TestPlatformNetworkInvalidCIDR(t *testing.T) {
	t.Setenv("K8S_NODE_CIDRS", "not-a-cidr")
	if _, err := FromEnv(); err == nil {
		t.Error("expected error for invalid K8S_NODE_CIDRS")
	} else if !strings.Contains(err.Error(), "K8S_NODE_CIDRS") {
		t.Errorf("error should mention K8S_NODE_CIDRS, got: %v", err)
	}
}

func TestPlatformNetworkInvalidPort(t *testing.T) {
	t.Setenv("MOCK_SERVICE_PORTS", "0,99999")
	if _, err := FromEnv(); err == nil {
		t.Error("expected error for invalid MOCK_SERVICE_PORTS")
	} else if !strings.Contains(err.Error(), "MOCK_SERVICE_PORTS") {
		t.Errorf("error should mention MOCK_SERVICE_PORTS, got: %v", err)
	}
}
