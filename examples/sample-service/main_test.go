package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/serviceopenapi"
)

func do(t *testing.T, h http.Handler, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestHealthz(t *testing.T) {
	rec := do(t, newServer(0, 0).routes(), http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

func TestTodosRoundTrip(t *testing.T) {
	h := newServer(0, 0).routes()

	rec := do(t, h, http.MethodGet, "/todos", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var listed struct {
		Todos []todo `json:"todos"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listed.Count != 2 || len(listed.Todos) != 2 {
		t.Fatalf("seed todos = %+v", listed)
	}

	rec = do(t, h, http.MethodPost, "/todos", `{"text":"new"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created todo
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == 0 || created.Text != "new" {
		t.Fatalf("created = %+v", created)
	}

	rec = do(t, h, http.MethodGet, "/todos", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if listed.Count != 3 {
		t.Fatalf("count after create = %d, want 3", listed.Count)
	}
}

func TestCreateTodoRejectsEmpty(t *testing.T) {
	rec := do(t, newServer(0, 0).routes(), http.MethodPost, "/todos", `{"text":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text = %d, want 400", rec.Code)
	}
}

// TestCreateTodoIsConcurrencySafe is the reason the sample exists: the write
// path must survive a real load test (run with -race).
func TestCreateTodoIsConcurrencySafe(t *testing.T) {
	h := newServer(0, 0).routes()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := do(t, h, http.MethodPost, "/todos", `{"text":"c"}`); rec.Code != http.StatusCreated {
				t.Errorf("concurrent create = %d", rec.Code)
			}
		}()
	}
	wg.Wait()
}

func TestSlowHonoursAndClamps(t *testing.T) {
	h := newServer(30*time.Millisecond, 0).routes()

	start := time.Now()
	rec := do(t, h, http.MethodGet, "/slow?ms=60", "")
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("/slow?ms=60 slept %v", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("slow = %d", rec.Code)
	}

	// Clamp: 99999ms must not sleep for a minute.
	start = time.Now()
	rec = do(t, h, http.MethodGet, "/slow?ms=99999", "")
	if elapsed := time.Since(start); elapsed > maxSlowMs*time.Millisecond+2*time.Second {
		t.Fatalf("clamp failed: slept %v", elapsed)
	}
	var out struct {
		SleptMS int `json:"slept_ms"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.SleptMS != maxSlowMs {
		t.Fatalf("slept_ms = %d, want %d", out.SleptMS, maxSlowMs)
	}

	// Default comes from the -latency-ms flag.
	start = time.Now()
	do(t, h, http.MethodGet, "/slow", "")
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("default latency not applied: %v", elapsed)
	}
}

func TestFlakyDeterministicAtBoundaries(t *testing.T) {
	h := newServer(0, 0).routes()
	if rec := do(t, h, http.MethodGet, "/flaky?rate=1", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("rate=1 = %d, want 500", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/flaky?rate=0", ""); rec.Code != http.StatusOK {
		t.Fatalf("rate=0 = %d, want 200", rec.Code)
	}
}

// TestOpenAPIIndexesFourTargets keeps the example honest: the platform's
// target indexer is what turns this spec into load-test targets, so a spec
// change that breaks indexing must fail here, not in the E2E job.
func TestOpenAPIIndexesFourTargets(t *testing.T) {
	spec, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	sum, err := serviceopenapi.ValidateAndIndex(spec)
	if err != nil {
		t.Fatalf("spec does not validate: %v", err)
	}
	if sum.Title != "Sample Service" {
		t.Errorf("title = %q", sum.Title)
	}
	want := []string{"createTodo", "flaky", "healthz", "listTodos", "slow"}
	var got []string
	for _, tg := range sum.Targets {
		got = append(got, tg.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] { // sorted by id
			t.Fatalf("targets = %v, want %v", got, want)
		}
	}
	if sum.OpCount != 5 {
		t.Errorf("op count = %d, want 5", sum.OpCount)
	}
}
