package config

import (
	"strings"
	"testing"
	"time"
)

// TestLoadLimitsDefaults — plans/42: the caps are always set, so the handler
// can never fall back to "unlimited" by accident.
func TestLoadLimitsDefaults(t *testing.T) {
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.MaxVUs != 500 {
		t.Errorf("MaxVUs = %d, want 500", c.MaxVUs)
	}
	if c.MaxDuration != 30*time.Minute {
		t.Errorf("MaxDuration = %v, want 30m", c.MaxDuration)
	}
	if c.MaxTimeoutMinutes != 60 {
		t.Errorf("MaxTimeoutMinutes = %d, want 60", c.MaxTimeoutMinutes)
	}
}

func TestLoadLimitsOverrides(t *testing.T) {
	t.Setenv("MAX_VUS", "10")
	t.Setenv("MAX_DURATION", "45s")
	t.Setenv("MAX_TIMEOUT_MINUTES", "5")
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.MaxVUs != 10 {
		t.Errorf("MaxVUs = %d, want 10", c.MaxVUs)
	}
	if c.MaxDuration != 45*time.Second {
		t.Errorf("MaxDuration = %v, want 45s", c.MaxDuration)
	}
	if c.MaxTimeoutMinutes != 5 {
		t.Errorf("MaxTimeoutMinutes = %d, want 5", c.MaxTimeoutMinutes)
	}
}

// TestLoadLimitsInvalid — fail closed at startup: an operator typo must not
// silently disable the guardrail.
func TestLoadLimitsInvalid(t *testing.T) {
	cases := []struct {
		env   string
		value string
	}{
		{"MAX_VUS", "0"},
		{"MAX_VUS", "lots"},
		{"MAX_DURATION", "banana"},
		{"MAX_DURATION", "30"},
		{"MAX_TIMEOUT_MINUTES", "-1"},
	}
	for _, tc := range cases {
		t.Run(tc.env+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			if _, err := FromEnv(); err == nil {
				t.Fatalf("expected error for %s=%q", tc.env, tc.value)
			} else if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error should mention %s, got: %v", tc.env, err)
			}
		})
	}
}
