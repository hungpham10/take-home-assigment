package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hungpham/loadtest-platform/pkgs/version"
)

// withVersion pins the build version for the duration of a test.
func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := version.Version
	t.Cleanup(func() { version.Version = prev })
	version.Version = v
}

func TestHealthEndpoint(t *testing.T) {
	withVersion(t, "0.1.1")
	h := NewRouter(Deps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("health status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("health body is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("health status field = %q, want ok", body["status"])
	}
	// /health carries the version so a probe/curl proves which build answers
	// (docs/versioning.md §2) — additive field, probe contract unchanged.
	if body["version"] != "0.1.1" {
		t.Errorf("health version = %q, want 0.1.1", body["version"])
	}
}

func TestVersionEndpoint(t *testing.T) {
	withVersion(t, "0.1.1")
	h := NewRouter(Deps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("version status = %d, want 200", rec.Code)
	}
	var info version.Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("version body is not JSON: %v", err)
	}
	if info.Version != "0.1.1" {
		t.Errorf("version = %q, want 0.1.1", info.Version)
	}
	if info.GoVersion == "" {
		t.Error("go_version must be set")
	}
}

func TestReadyFailsClosedWithoutDB(t *testing.T) {
	h := NewRouter(Deps{}) // no pool -> NOT_READY
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("ready status = %d, want 503", rec.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := NewRouter(Deps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("metrics status = %d, want 200", rec.Code)
	}
}
