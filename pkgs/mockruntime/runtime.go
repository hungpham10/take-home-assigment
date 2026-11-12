// Package mockruntime — the data-plane mock runtime (plans/21 PR D,
// docs/mock-service-api.md + docs/k8s-orchestration.md §8). It is a SEPARATE
// deployment unit (cmds/mock-service): it holds NO database, makes NO authz
// decisions, and never writes PG. Desired state arrives as a versioned
// snapshot (binding ConfigMap in kind; file/env locally) and is swapped
// atomically; the runtime reports observed_generation and health for the
// watcher's mock evaluator (MOCK-VER-01).
package mockruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync/atomic"
	"time"
)

// SchemaVersion of the snapshot contract (controller → mock-service).
const SchemaVersion = "v1"

// Snapshot is the versioned desired config: routes carry variants, weights,
// latency profiles and error classes.
type Snapshot struct {
	SchemaVersion string  `json:"schema_version"`
	NamespaceID   string  `json:"namespace_id"`
	Generation    int64   `json:"generation"`
	Routes        []Route `json:"routes"`
}

// Route matches one method+path.
type Route struct {
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Variants []Variant `json:"variants"`
}

// Variant is one weighted outcome: status/body, latency profile or error.
type Variant struct {
	Weight  int      `json:"weight"`
	Status  int      `json:"status"`
	Body    Body     `json:"body"`
	Latency *Latency `json:"latency,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Body accepts raw JSON templates (validated at decode, emitted verbatim).
type Body json.RawMessage

// UnmarshalJSON validates the body is a JSON value.
func (b *Body) UnmarshalJSON(data []byte) error {
	if !json.Valid(data) {
		return errors.New("mockruntime: body template is not valid JSON")
	}
	*b = Body(data)
	return nil
}

// MarshalJSON emits the stored JSON; an empty body emits null.
func (b Body) MarshalJSON() ([]byte, error) {
	if len(b) == 0 {
		return []byte("null"), nil
	}
	return json.RawMessage(b), nil
}

// Latency is the injection profile: fixed ms or a percentile-based shape
// approximated by triangular interpolation between p50 and p95.
type Latency struct {
	Distribution string `json:"distribution"` // fixed | lognormal
	MS           int64  `json:"ms"`           // fixed mode
	P50MS        int64  `json:"p50_ms"`       // lognormal mode
	P95MS        int64  `json:"p95_ms"`
	MinMS        int64  `json:"min_ms"`
	MaxMS        int64  `json:"max_ms"`
}

// Runtime holds the current snapshot behind an atomic pointer (hot path has
// no locks and no PG — docs/mock-service-api.md "no DB writes on hot path").
type Runtime struct {
	snap atomic.Pointer[Snapshot]
	rng  *rand.Rand
}

// New returns an empty runtime.
func New() *Runtime {
	return &Runtime{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// ObservedGeneration reports the generation of the currently applied
// snapshot (0 = nothing applied).
func (r *Runtime) ObservedGeneration() int64 {
	s := r.snap.Load()
	if s == nil {
		return 0
	}
	return s.Generation
}

// ApplySnapshot atomically swaps in the snapshot. Older generations are
// rejected (generation fencing on the data plane, MOCK-VER-01); the first
// snapshot after a restart is accepted regardless of ordering.
func (r *Runtime) ApplySnapshot(s *Snapshot) error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("mockruntime: unsupported schema_version %q (want %s)", s.SchemaVersion, SchemaVersion)
	}
	if s.Generation < 0 {
		return errors.New("mockruntime: negative generation")
	}
	if cur := r.snap.Load(); cur != nil && s.Generation < cur.Generation {
		return fmt.Errorf("mockruntime: stale snapshot gen %d < current %d", s.Generation, cur.Generation)
	}
	r.snap.Store(s)
	return nil
}

// LoadFile reads a snapshot JSON file (local mode; kind mounts the binding
// ConfigMap to a file path).
func LoadFile(r *Runtime, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("mockruntime: read snapshot: %w", err)
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("mockruntime: decode snapshot: %w", err)
	}
	return r.ApplySnapshot(&s)
}

// Handler returns the HTTP mux serving the snapshot routes.
func (r *Runtime) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", r.handleHealth)
	mux.HandleFunc("/", r.handleRoute)
	return mux
}

func (r *Runtime) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":              "healthy",
		"observed_generation": r.ObservedGeneration(),
	})
}

func (r *Runtime) handleRoute(w http.ResponseWriter, req *http.Request) {
	s := r.snap.Load()
	if s == nil {
		noRoute(w, "no_snapshot")
		return
	}
	for i := range s.Routes {
		rt := &s.Routes[i]
		if rt.Method == req.Method && rt.Path == req.URL.Path {
			r.serveVariant(w, s, rt)
			return
		}
	}
	noRoute(w, "no_route")
}

func noRoute(w http.ResponseWriter, reason string) {
	w.Header().Set("X-Mock-Reason", reason)
	writeJSON(w, http.StatusNotFound, map[string]string{"error": reason})
}

// serveVariant picks a weighted variant deterministically-randomly, injects
// latency, and emits the configured outcome.
func (r *Runtime) serveVariant(w http.ResponseWriter, s *Snapshot, rt *Route) {
	w.Header().Set("X-Mock-Generation", fmt.Sprintf("%d", s.Generation))
	v := pickVariant(r.rng, rt.Variants)
	if v == nil {
		noRoute(w, "no_variant")
		return
	}
	injectLatency(r.rng, v.Latency)
	if v.Error != "" {
		w.Header().Set("X-Mock-Error", v.Error)
		writeJSON(w, statusOr(v.Status, 500), map[string]string{"error": v.Error})
		return
	}
	writeJSON(w, statusOr(v.Status, 200), bodyOr(v.Body))
}

func pickVariant(rng *rand.Rand, variants []Variant) *Variant {
	total := 0
	for i := range variants {
		if variants[i].Weight > 0 {
			total += variants[i].Weight
		}
	}
	if total == 0 {
		if len(variants) > 0 {
			return &variants[0]
		}
		return nil
	}
	roll := rng.Intn(total)
	for i := range variants {
		if variants[i].Weight <= 0 {
			continue
		}
		roll -= variants[i].Weight
		if roll < 0 {
			return &variants[i]
		}
	}
	return &variants[len(variants)-1]
}

// injectLatency applies the profile: fixed, or triangular interpolation
// between p50..p95 (deterministic bounds, no heavy distributions on the hot
// path) clamped to min/max.
func injectLatency(rng *rand.Rand, l *Latency) {
	if l == nil {
		return
	}
	var ms int64
	switch l.Distribution {
	case "fixed", "":
		ms = l.MS
	case "lognormal":
		if l.P50MS <= 0 || l.P95MS <= l.P50MS {
			ms = l.P50MS
		} else {
			// Triangular[p50, p95] with mode p50: cheap percentile-shape
			// approximation of the injected delay.
			u := rng.Float64()
			lo, hi := float64(l.P50MS), float64(l.P95MS)
			ms = int64(lo + (hi-lo)*u*u)
		}
	}
	if l.MinMS > 0 && ms < l.MinMS {
		ms = l.MinMS
	}
	if l.MaxMS > 0 && ms > l.MaxMS {
		ms = l.MaxMS
	}
	if ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
}

func statusOr(s, def int) int {
	if s == 0 {
		return def
	}
	return s
}

func bodyOr(b Body) any {
	if len(b) == 0 {
		return map[string]any{}
	}
	return json.RawMessage(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// SortRoutes keeps route ordering stable for snapshot diffs (debug helper).
func SortRoutes(s *Snapshot) {
	sort.Slice(s.Routes, func(i, j int) bool {
		return s.Routes[i].Path < s.Routes[j].Path
	})
}
