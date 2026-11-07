package config

import "testing"

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
