// Command sample-service is a minimal system-under-test for the load-testing
// platform demo (plans/43). It exists so the E2E path, the OpenAPI target
// index and the k6 result thresholds can be exercised against a service whose
// behaviour is known, instead of a third-party echo image.
//
// Deliberately dependency-free and in-memory: no database, no auth.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxSlowMs bounds /slow so a single request cannot pin a connection for a
// minute (the load test's own timeout is minutes, not seconds).
const maxSlowMs = 2000

type todo struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

// server holds the in-memory state. The mutex protects todos and the RNG:
// POST /todos is a write path, so the load test exercises real contention.
type server struct {
	mu        sync.Mutex
	todos     []todo
	nextID    int
	latency   time.Duration
	errorRate float64
	rng       *rand.Rand
}

func newServer(latency time.Duration, errorRate float64) *server {
	return &server{
		nextID:    3,
		latency:   latency,
		errorRate: errorRate,
		rng:       rand.New(rand.NewPCG(1, 2)), // fixed seed: reproducible tests
		todos: []todo{
			{ID: 1, Text: "write the load test"},
			{ID: 2, Text: "read the result summary"},
		},
	}
}

// routes wires the four documented operations (examples/sample-service/openapi.yaml).
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /todos", s.listTodos)
	mux.HandleFunc("POST /todos", s.createTodo)
	mux.HandleFunc("GET /slow", s.slow)
	mux.HandleFunc("GET /flaky", s.flaky)
	return mux
}

func (s *server) listTodos(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	out := make([]todo, len(s.todos))
	copy(out, s.todos)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"todos": out, "count": len(out)})
}

func (s *server) createTodo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || in.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required"})
		return
	}
	s.mu.Lock()
	t := todo{ID: s.nextID, Text: in.Text}
	s.nextID++
	s.todos = append(s.todos, t)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, t)
}

// slow returns after a bounded sleep; the platform demo uses it to make p95
// assertions deterministic instead of hoping for natural latency.
func (s *server) slow(w http.ResponseWriter, r *http.Request) {
	ms := intFromQuery(r, "ms", defaultSlowMs(s))
	time.Sleep(time.Duration(ms) * time.Millisecond)
	writeJSON(w, http.StatusOK, map[string]any{"slept_ms": ms})
}

// flaky fails with the requested probability. rate=1 is deterministic and is
// what the failing-threshold fixture (plans/45) uses.
func (s *server) flaky(w http.ResponseWriter, r *http.Request) {
	rate := floatFromQuery(r, "rate", s.errorRate)
	s.mu.Lock()
	roll := s.rng.Float64()
	s.mu.Unlock()
	if roll < rate {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "flaky"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func defaultSlowMs(s *server) int {
	return int(s.latency / time.Millisecond)
}

func intFromQuery(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	if n > maxSlowMs {
		return maxSlowMs
	}
	return n
}

func floatFromQuery(r *http.Request, key string, def float64) float64 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 {
		return def
	}
	if f > 1 {
		return 1
	}
	return f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	latencyMs := flag.Int("latency-ms", 0, "baseline latency added by GET /slow when ?ms= is absent")
	errorRate := flag.Float64("error-rate", 0, "500 probability for GET /flaky when ?rate= is absent (0..1)")
	// Container healthcheck: distroless has no shell/curl, so the binary probes
	// itself. `sample-service -probe http://127.0.0.1:8080` exits 0 when healthy.
	probeURL := flag.String("probe", "", "probe mode: GET <url>/healthz and exit 0/1")
	flag.Parse()

	if *probeURL != "" {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(strings.TrimRight(*probeURL, "/") + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			log.Fatalf("probe failed: %v", err)
		}
		_ = resp.Body.Close()
		return
	}

	if *latencyMs < 0 || *latencyMs > maxSlowMs {
		log.Fatalf("latency-ms must be between 0 and %d", maxSlowMs)
	}
	if *errorRate < 0 || *errorRate > 1 {
		log.Fatal("error-rate must be between 0 and 1")
	}

	s := newServer(time.Duration(*latencyMs)*time.Millisecond, *errorRate)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("sample-service listening on %s (latency=%dms, error-rate=%.2f)\n", *addr, *latencyMs, *errorRate)
	log.Fatal(srv.ListenAndServe())
}
