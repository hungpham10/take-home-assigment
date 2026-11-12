package loadtest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/loadtest"
)

// limitsFixture mirrors the config defaults (plans/42).
func limitsFixture() loadtest.Limits {
	return loadtest.Limits{MaxVUs: 500, MaxDuration: 30 * time.Minute, MaxTimeoutMinutes: 60}
}

// cfgFromJSON decodes a request-shaped load_config the same way the handler
// does (numbers arrive as float64), so the validation tests cover real input.
func cfgFromJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	cfg := map[string]any{"vusers": 200, "duration": "10m", "timeout_minutes": 15}
	if raw == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("bad fixture %s: %v", raw, err)
	}
	return cfg
}

func TestValidateLoadConfig(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		limits   loadtest.Limits
		wantCode string
		wantMsg  []string // substrings that must appear in the message
	}{
		{name: "defaults pass", raw: ""},
		{name: "at vus limit", raw: `{"vusers":500}`},
		{name: "over vus limit", raw: `{"vusers":501}`, wantCode: "VUS_LIMIT_EXCEEDED", wantMsg: []string{"501", "500"}},
		{name: "zero vus", raw: `{"vusers":0}`, wantCode: "VALIDATION_FAILED"},
		{name: "negative vus", raw: `{"vusers":-3}`, wantCode: "VALIDATION_FAILED"},
		{name: "non-numeric vus", raw: `{"vusers":"many"}`, wantCode: "VALIDATION_FAILED"},
		{name: "seconds duration", raw: `{"duration":"30s","timeout_minutes":15}`},
		{name: "compound duration", raw: `{"duration":"1h30m","timeout_minutes":120}`, limits: loadtest.Limits{MaxVUs: 500, MaxDuration: 2 * time.Hour, MaxTimeoutMinutes: 180}},
		{name: "garbage duration", raw: `{"duration":"banana"}`, wantCode: "INVALID_DURATION", wantMsg: []string{"banana"}},
		{name: "unitless duration", raw: `{"duration":"10"}`, wantCode: "INVALID_DURATION"},
		{name: "duration too short", raw: `{"duration":"1s"}`, wantCode: "VALIDATION_FAILED", wantMsg: []string{"10s"}},
		{name: "at duration limit", raw: `{"duration":"30m","timeout_minutes":40}`},
		{name: "over duration limit", raw: `{"duration":"31m","timeout_minutes":40}`, wantCode: "DURATION_LIMIT_EXCEEDED", wantMsg: []string{"31m0s", "30m0s"}},
		{name: "zero timeout", raw: `{"timeout_minutes":0}`, wantCode: "VALIDATION_FAILED"},
		{name: "over timeout limit", raw: `{"timeout_minutes":61}`, wantCode: "TIMEOUT_LIMIT_EXCEEDED", wantMsg: []string{"61", "60"}},
		{name: "timeout not above duration", raw: `{"duration":"10m","timeout_minutes":5}`, wantCode: "TIMEOUT_TOO_SHORT", wantMsg: []string{"5", "10m0s"}},
		{name: "non-numeric timeout", raw: `{"timeout_minutes":"soon"}`, wantCode: "VALIDATION_FAILED"},
		{name: "custom max vus", raw: `{"vusers":11}`, limits: loadtest.Limits{MaxVUs: 10, MaxDuration: time.Hour, MaxTimeoutMinutes: 60}, wantCode: "VUS_LIMIT_EXCEEDED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limits := tc.limits
			if limits.MaxVUs == 0 { // zero-value → fixture defaults
				limits = limitsFixture()
			}
			code, msg := loadtest.ValidateLoadConfig(cfgFromJSON(t, tc.raw), limits)
			if code != tc.wantCode {
				t.Fatalf("code = %q (%s), want %q", code, msg, tc.wantCode)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q should contain %q", msg, want)
				}
			}
		})
	}
}

// TestLimitsWithDefaults — a composition that forgets to set Limits must still
// be guarded (zero-value = documented defaults, never "unlimited").
func TestLimitsWithDefaults(t *testing.T) {
	l := loadtest.Limits{}.WithDefaults()
	if l.MaxVUs != 500 || l.MaxDuration != 30*time.Minute || l.MaxTimeoutMinutes != 60 {
		t.Fatalf("WithDefaults = %+v", l)
	}
	code, _ := loadtest.ValidateLoadConfig(map[string]any{"vusers": 501, "duration": "10m", "timeout_minutes": 15}, loadtest.Limits{})
	if code != "VUS_LIMIT_EXCEEDED" {
		t.Errorf("zero Limits must still cap: code = %q", code)
	}
}
