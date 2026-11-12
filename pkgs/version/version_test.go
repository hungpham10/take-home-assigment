package version

import (
	"bytes"
	"encoding/json"
	"runtime"
	"runtime/debug"
	"testing"
)

// withBuildInfo stubs the debug.ReadBuildInfo hook and restores it after the
// test. Deterministic: a real `go test` run in a git checkout also carries
// vcs.* settings, which would otherwise leak into the fallback assertions.
func withBuildInfo(t *testing.T, ok bool, settings map[string]string) {
	t.Helper()
	prev := readBuildInfo
	t.Cleanup(func() { readBuildInfo = prev })
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		if !ok {
			return nil, false
		}
		bi := &debug.BuildInfo{}
		for k, v := range settings {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: k, Value: v})
		}
		return bi, true
	}
}

// withVars saves/restores the ldflags-injected package variables.
func withVars(t *testing.T) {
	t.Helper()
	prevVersion, prevCommit, prevDate := Version, Commit, BuildDate
	t.Cleanup(func() { Version, Commit, BuildDate = prevVersion, prevCommit, prevDate })
}

func TestGetInjectedValuesWinOverBuildInfo(t *testing.T) {
	withVars(t)
	withBuildInfo(t, true, map[string]string{
		"vcs.revision": "deadbeef",
		"vcs.time":     "2000-01-01T00:00:00Z",
	})
	Version, Commit, BuildDate = "0.1.1", "1ead5d4", "2026-11-12T09:00:00Z"

	got := Get()
	if got.Version != "0.1.1" || got.Commit != "1ead5d4" || got.BuildDate != "2026-11-12T09:00:00Z" {
		t.Fatalf("Get() = %+v, want injected values", got)
	}
	if got.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", got.GoVersion, runtime.Version())
	}
}

func TestGetFallsBackToBuildInfo(t *testing.T) {
	withVars(t)
	withBuildInfo(t, true, map[string]string{
		"vcs.revision": "abc1234567890",
		"vcs.time":     "2026-11-12T09:00:00Z",
	})
	Version, Commit, BuildDate = "0.1.1", "", ""

	got := Get()
	if got.Commit != "abc1234" {
		t.Errorf("Commit = %q, want short revision abc1234", got.Commit)
	}
	if got.BuildDate != "2026-11-12T09:00:00Z" {
		t.Errorf("BuildDate = %q, want vcs.time", got.BuildDate)
	}
}

func TestGetDoesNotOverrideInjectedCommit(t *testing.T) {
	withVars(t)
	withBuildInfo(t, true, map[string]string{
		"vcs.revision": "abc1234567890",
		"vcs.time":     "2026-11-12T09:00:00Z",
	})
	Version, Commit, BuildDate = "0.1.1", "manual1", "2026-01-01T00:00:00Z"

	got := Get()
	if got.Commit != "manual1" || got.BuildDate != "2026-01-01T00:00:00Z" {
		t.Fatalf("Get() = %+v, injected commit/date must win", got)
	}
}

func TestGetDefaultsVersionToDev(t *testing.T) {
	withVars(t)
	withBuildInfo(t, false, nil)
	Version, Commit, BuildDate = "", "", ""

	got := Get()
	if got.Version != "dev" {
		t.Errorf("Version = %q, want dev", got.Version)
	}
	if got.GoVersion == "" {
		t.Error("GoVersion must never be empty")
	}
}

func TestGetSurvivesNoBuildInfo(t *testing.T) {
	withVars(t)
	withBuildInfo(t, false, nil)
	Version, Commit, BuildDate = "0.1.1", "", ""

	got := Get()
	if got.Version != "0.1.1" {
		t.Errorf("Version = %q, want 0.1.1", got.Version)
	}
	if got.Commit != "" || got.BuildDate != "" {
		t.Errorf("Commit/BuildDate = %q/%q, want empty without build info", got.Commit, got.BuildDate)
	}
}

func TestString(t *testing.T) {
	withVars(t)
	withBuildInfo(t, false, nil)

	cases := []struct {
		name                       string
		version, commit, buildDate string
		want                       string
	}{
		{"dev default", "", "", "", "dev"},
		{"dev with commit", "", "1ead5d4", "", "dev (1ead5d4)"},
		{"version only", "0.1.1", "", "", "v0.1.1"},
		{"full", "0.1.1", "1ead5d4", "2026-11-12T09:00:00Z", "v0.1.1 (1ead5d4, 2026-11-12T09:00:00Z)"},
		{"already prefixed", "v0.1.1", "1ead5d4", "", "v0.1.1 (1ead5d4)"},
		{"commit no date", "0.1.1", "1ead5d4", "", "v0.1.1 (1ead5d4)"},
		{"date no commit", "0.1.1", "", "2026-11-12T09:00:00Z", "v0.1.1 (2026-11-12T09:00:00Z)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			Version, Commit, BuildDate = tc.version, tc.commit, tc.buildDate
			if got := String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInfoJSONKeys(t *testing.T) {
	withVars(t)
	withBuildInfo(t, false, nil)
	Version, Commit, BuildDate = "0.1.1", "1ead5d4", "2026-11-12T09:00:00Z"

	b, err := json.Marshal(Get())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "commit", "build_date", "go_version"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("JSON missing key %q: %s", key, b)
		}
	}
}

func TestPrintIfRequested(t *testing.T) {
	withVars(t)
	withBuildInfo(t, false, nil)
	Version, Commit, BuildDate = "0.1.1", "1ead5d4", ""

	cases := []struct {
		name    string
		args    []string
		want    bool
		wantOut string
	}{
		{"no args", nil, false, ""},
		{"other flag", []string{"sops", "encrypt"}, false, ""},
		{"long flag", []string{"--version"}, true, "v0.1.1 (1ead5d4)\n"},
		{"single dash", []string{"-version"}, true, "v0.1.1 (1ead5d4)\n"},
		{"subcommand", []string{"version"}, true, "v0.1.1 (1ead5d4)\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if got := PrintIfRequested(tc.args, &buf); got != tc.want {
				t.Fatalf("PrintIfRequested(%v) = %v, want %v", tc.args, got, tc.want)
			}
			if buf.String() != tc.wantOut {
				t.Errorf("output = %q, want %q", buf.String(), tc.wantOut)
			}
		})
	}
}
