package mockruntime_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/mockruntime"
)

func snapshot() *mockruntime.Snapshot {
	return &mockruntime.Snapshot{
		SchemaVersion: "v1",
		NamespaceID:   "ns-1",
		Generation:    7,
		Routes: []mockruntime.Route{{
			Method: "GET", Path: "/widgets",
			Variants: []mockruntime.Variant{{
				Weight: 1, Status: 200,
				Body:    mockruntime.Body(`{"widgets": []}`),
				Latency: &mockruntime.Latency{Distribution: "fixed", MS: 20},
			}},
		}, {
			Method: "POST", Path: "/widgets",
			Variants: []mockruntime.Variant{
				{Weight: 9, Status: 201, Body: mockruntime.Body(`{"ok": true}`)},
				{Weight: 1, Status: 503, Error: "pool_exhausted"},
			},
		}},
	}
}

func TestSnapshotSwapAndGeneration(t *testing.T) {
	rt := mockruntime.New()
	if rt.ObservedGeneration() != 0 {
		t.Fatal("empty runtime reports generation 0")
	}
	_ = rt.ApplySnapshot(snapshot())
	if rt.ObservedGeneration() != 7 {
		t.Fatalf("observed_generation %d", rt.ObservedGeneration())
	}
	// Atomic swap: newer generation wins, older is rejected.
	if err := rt.ApplySnapshot(&mockruntime.Snapshot{Generation: 5}); err == nil {
		t.Fatal("older snapshot accepted")
	}
	if rt.ObservedGeneration() != 7 {
		t.Fatal("older snapshot applied")
	}
}

func TestServeRoutesAndLatency(t *testing.T) {
	rt := mockruntime.New()
	_ = rt.ApplySnapshot(snapshot())
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()

	start := time.Now()
	rec, err := http.Get(srv.URL + "/widgets")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rec.Body.Close() }()
	if rec.StatusCode != 200 {
		t.Fatalf("status %d", rec.StatusCode)
	}
	if rec.Header.Get("X-Mock-Generation") != "7" {
		t.Fatalf("generation header %q", rec.Header.Get("X-Mock-Generation"))
	}
	var body map[string]any
	if json.NewDecoder(rec.Body).Decode(&body) != nil || body["widgets"] == nil {
		t.Fatalf("body %v", body)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("latency not injected: %v", elapsed)
	}
}

func TestServeUnknownRoute(t *testing.T) {
	rt := mockruntime.New()
	_ = rt.ApplySnapshot(snapshot())
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()
	rec, _ := http.Get(srv.URL + "/nope")
	defer func() { _ = rec.Body.Close() }()
	if rec.StatusCode != 404 {
		t.Fatalf("unknown route: %d", rec.StatusCode)
	}
	if !strings.Contains(rec.Header.Get("X-Mock-Reason"), "no_route") {
		t.Fatalf("reason header %q", rec.Header.Get("X-Mock-Reason"))
	}
}

func TestHealthEndpoint(t *testing.T) {
	rt := mockruntime.New()
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()
	rec, _ := http.Get(srv.URL + "/health")
	defer func() { _ = rec.Body.Close() }()
	if rec.StatusCode != 200 {
		t.Fatalf("health: %d", rec.StatusCode)
	}
	var h struct {
		Status     string `json:"status"`
		Generation int64  `json:"observed_generation"`
	}
	if json.NewDecoder(rec.Body).Decode(&h) != nil || h.Status != "healthy" {
		t.Fatalf("health payload %+v", h)
	}
}

func TestLoadFromFile(t *testing.T) {
	snap := snapshot()
	rt := mockruntime.New()
	if err := mockruntime.LoadFile(rt, "/nonexistent.json"); err == nil {
		t.Fatal("missing file must error")
	}
	// valid file round-trips through the same decoder
	b, _ := json.Marshal(snap)
	f := t.TempDir() + "/snap.json"
	if err := writeFile(f, b); err != nil {
		t.Fatal(err)
	}
	if err := mockruntime.LoadFile(rt, f); err != nil {
		t.Fatalf("load file: %v", err)
	}
	if rt.ObservedGeneration() != 7 {
		t.Fatalf("generation after file load: %d", rt.ObservedGeneration())
	}
}
