package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// exec runs the CLI in-process against srv (nil = no server needed) and returns
// the exit code plus captured streams. It never touches os.Stdout.
func exec(t *testing.T, srv *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	if srv != nil {
		args = append([]string{"--server", srv.URL, "--poll", "1ms", "--wait-timeout", "2s"}, args...)
	}
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// --- exit-code contract ----------------------------------------------------

// TestExitCodes pins the contract CI depends on.
func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"help", []string{"--help"}, exitOK},
		{"no args", nil, exitOK},
		{"version", []string{"version"}, exitOK},
		{"unknown command", []string{"bogus"}, exitUsage},
		{"unknown group action", []string{"ns", "bogus"}, exitUsage},
		{"missing argument", []string{"ns", "show"}, exitUsage},
		{"missing required flag", []string{"run", "start", "ns-1"}, exitUsage},
		{"bad flag type", []string{"run", "start", "ns-1", "--vus", "many"}, exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := exec(t, nil, tc.args...)
			if code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
		})
	}
}

// TestTransportFailureIsRuntime — a closed port is exit 1, not 3.
func TestTransportFailureIsRuntime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	code, _, stderr := exec(t, nil, "--server", url, "ns", "list")
	if code != exitRuntime {
		t.Fatalf("exit = %d, want %d (stderr %s)", code, exitRuntime, stderr)
	}
}

// TestStateFailureIsThree — 409/422 maps to exit 3 so CI can distinguish
// "fix your config" from "the server is broken".
func TestStateFailureIsThree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"NAMESPACE_NOT_READY","message":"namespace is DRAFT"}}`)
	}))
	defer srv.Close()
	code, _, stderr := exec(t, srv, "run", "start", "ns-1", "--script", "sc-1")
	if code != exitState {
		t.Fatalf("exit = %d, want %d (stderr %s)", code, exitState, stderr)
	}
	if !strings.Contains(stderr, "NAMESPACE_NOT_READY") {
		t.Errorf("stderr lost the server code: %s", stderr)
	}
}

// --- ns wait ---------------------------------------------------------------

func TestNSWaitReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/loadtest/v1/namespaces/ns-1/readiness" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"namespace_id":"ns-1","status":"READY","ready":true,"ready_for_loadtest":true}`)
	}))
	defer srv.Close()
	code, stdout, stderr := exec(t, srv, "ns", "wait", "ns-1")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "READY") {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestNSWaitBlockersOnTimeout — a developer must see what is blocking, not just
// "timed out".
func TestNSWaitBlockersOnTimeout(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, `{"namespace_id":"ns-1","status":"APPLYING","ready":false,
			"blocking_reasons":["deploy/echo: waiting for rollout","database/demo-db: not ready"]}`)
	}))
	defer srv.Close()
	code, _, stderr := exec(t, nil, "--server", srv.URL, "--poll", "1ms", "--wait-timeout", "15ms",
		"ns", "wait", "ns-1")
	if code != exitState {
		t.Fatalf("exit = %d, want %d (stderr %s)", code, exitState, stderr)
	}
	for _, want := range []string{"status APPLYING", "blocking: deploy/echo: waiting for rollout", "blocking: database/demo-db: not ready"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if calls < 2 {
		t.Errorf("polls = %d, want >1 (wait must poll, not one-shot)", calls)
	}
}

// --- script push -----------------------------------------------------------

func TestScriptPushOversizeFailsBeforeUpload(t *testing.T) {
	var hit int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hit, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"sc-1","status":"VALIDATED"}`)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "big.js")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), MaxScriptSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := exec(t, srv, "script", "push", "ns-1", path)
	if code != exitState {
		t.Fatalf("exit = %d, want %d (stderr %s)", code, exitState, stderr)
	}
	if !strings.Contains(stderr, "1 MiB") {
		t.Errorf("stderr should name the limit: %s", stderr)
	}
	if hit != 0 {
		t.Error("oversize script reached the server; the limit must be enforced client-side")
	}
}

func TestScriptPushWaitsForValidation(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scripts"):
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"sc-1","name":"k6.js","status":"PENDING_VALIDATION"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/scripts/sc-1"):
			if atomic.AddInt32(&polls, 1) == 1 {
				_, _ = io.WriteString(w, `{"id":"sc-1","status":"VALIDATING"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"sc-1","status":"VALIDATED"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "k6.js")
	if err := os.WriteFile(path, []byte("export default function(){}"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := exec(t, srv, "script", "push", "ns-1", path)
	if code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "VALIDATED") || !strings.Contains(stdout, "ltp run start") {
		t.Errorf("stdout = %q", stdout)
	}
	if polls < 2 {
		t.Errorf("polls = %d, want ≥2 (must wait for terminal validation)", polls)
	}
}

func TestScriptPushInvalidIsStateFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"sc-1","status":"PENDING_VALIDATION"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"sc-1","status":"INVALID"}`)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "k6.js")
	if err := os.WriteFile(path, []byte("syntax error"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := exec(t, srv, "script", "push", "ns-1", path)
	if code != exitState {
		t.Fatalf("exit = %d, want %d", code, exitState)
	}
	if !strings.Contains(stderr, "not validated") {
		t.Errorf("stderr = %s", stderr)
	}
}

// --- run watch -------------------------------------------------------------

func TestRunWatchCompletedIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"lt-1","status":"COMPLETED"}`)
	}))
	defer srv.Close()
	code, stdout, stderr := exec(t, srv, "run", "watch", "ns-1", "lt-1")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "COMPLETED") {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestRunWatchFailedIsThree — the run reached a terminal state, so this is a
// state failure (the test did not pass), not a runtime error.
func TestRunWatchFailedIsThree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"lt-1","status":"FAILED"}`)
	}))
	defer srv.Close()
	code, _, stderr := exec(t, srv, "run", "watch", "ns-1", "lt-1")
	if code != exitState {
		t.Fatalf("exit = %d, want %d", code, exitState)
	}
	if !strings.Contains(stderr, "not COMPLETED") {
		t.Errorf("stderr = %s", stderr)
	}
}

// TestRunWatchPollsUntilTerminal — RUNNING is not terminal.
func TestRunWatchPollsUntilTerminal(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		status := "RUNNING"
		if atomic.AddInt32(&polls, 1) > 2 {
			status = "COMPLETED"
		}
		_, _ = io.WriteString(w, `{"id":"lt-1","status":"`+status+`"}`)
	}))
	defer srv.Close()
	code, _, stderr := exec(t, srv, "run", "watch", "ns-1", "lt-1")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	if polls != 3 {
		t.Errorf("polls = %d, want 3", polls)
	}
}

// --- run start -------------------------------------------------------------

func TestRunStartSendsLoadConfig(t *testing.T) {
	var body map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"lt-9","status":"PROVISIONING"}`)
	}))
	defer srv.Close()
	code, stdout, stderr := exec(t, srv, "run", "start", "ns-1", "--script", "sc-1",
		"--vus", "7", "--duration", "45s", "--timeout", "5", "--name", "smoke")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	if path != "/api/loadtest/v1/namespaces/ns-1/loadtests" {
		t.Errorf("path = %s", path)
	}
	cfg, _ := body["load_config"].(map[string]any)
	if cfg["vusers"] != float64(7) || cfg["duration"] != "45s" || cfg["timeout_minutes"] != float64(5) {
		t.Errorf("load_config = %v", body["load_config"])
	}
	if body["script_id"] != "sc-1" || body["name"] != "smoke" {
		t.Errorf("body = %v", body)
	}
	if !strings.Contains(stdout, "ltp run watch") {
		t.Errorf("stdout should print the next step: %q", stdout)
	}
}

// --- output contract -------------------------------------------------------

// TestJSONOutputIsOneObject — scripts pipe `ltp --json …` into jq; the stream
// must be exactly one JSON document with no prose mixed in.
func TestJSONOutputIsOneObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"ns-1","name":"demo","status":"READY","ready_for_loadtest":true,"version":2}`)
	}))
	defer srv.Close()
	code, stdout, stderr := exec(t, srv, "--json", "ns", "show", "ns-1")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Errorf("stdout has trailing content after the JSON object: %q", stdout)
	}
	if obj["id"] != "ns-1" || obj["ready_for_loadtest"] != true {
		t.Errorf("object = %v", obj)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty on success: %q", stderr)
	}
}

// TestTokenIsSent — the abt_ token must travel as a bearer header.
func TestTokenIsSent(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"namespaces":[],"count":0}`)
	}))
	defer srv.Close()
	code, _, stderr := exec(t, nil, "--server", srv.URL, "--token", "abt_abc", "ns", "list")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	if auth != "Bearer abt_abc" {
		t.Errorf("authorization = %q", auth)
	}
}

// TestUsageMentionsEveryCommand keeps the help text honest: docs/developer-cli.md
// mirrors it, and a command that exists but is undocumented is a doc bug.
func TestUsageMentionsEveryCommand(t *testing.T) {
	help := usage()
	for _, want := range []string{
		"ns create", "ns list", "ns show", "ns validate", "ns apply", "ns wait", "ns delete",
		"script push", "script show",
		"run start", "run watch", "run stop", "run report",
		"--server", "--token", "--json", "--wait-timeout", "Exit codes",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("usage() is missing %q", want)
		}
	}
}

// TestPollIntervalFlagIsHonoured guards the --poll plumbing (tests and demos
// depend on it; a silently ignored flag would make waits feel broken).
func TestPollIntervalFlagIsHonoured(t *testing.T) {
	var times []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		times = append(times, time.Now())
		status := "RUNNING"
		if len(times) > 1 {
			status = "COMPLETED"
		}
		_, _ = io.WriteString(w, `{"id":"lt-1","status":"`+status+`"}`)
	}))
	defer srv.Close()
	if code, _, stderr := exec(t, srv, "run", "watch", "ns-1", "lt-1"); code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	if len(times) != 2 {
		t.Fatalf("polls = %d, want 2", len(times))
	}
	if gap := times[1].Sub(times[0]); gap > 500*time.Millisecond {
		t.Errorf("gap between polls = %s; --poll 1ms was ignored", gap)
	}
}

// --- run report (plans/45) -------------------------------------------------

// TestRunReportRendersSummary — the report is the product's central output:
// it must show the numbers a developer acts on, and the failing threshold.
func TestRunReportRendersSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"id":"lt-1","name":"demo","status":"COMPLETED","version":3,
			"result_collected_at":"2026-11-12T10:04:11Z",
			"thresholds_failed":true,
			"result_summary":{
				"http_reqs":12480,"rps":41.6,"p95_ms":82,"p99_ms":141,"avg_ms":61.2,
				"error_rate":0.0031,"checks":12480,"checks_failed":39,
				"thresholds":[
					{"metric":"http_req_duration","name":"p(95)<200","ok":true},
					{"metric":"http_req_failed","name":"rate<0.01","ok":false}
				],
				"thresholds_failed":true
			}
		}`)
	}))
	defer srv.Close()
	code, stdout, stderr := exec(t, srv, "run", "report", "ns-1", "lt-1")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	for _, want := range []string{
		"12,480", "41.60", "p95 82ms", "p99 141ms", "avg 61.20ms",
		"0.31%", "39/12,480", "FAILED",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report missing %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "✓ http_req_duration p(95)<200") {
		t.Errorf("passing threshold not rendered with its metric:\n%s", stdout)
	}
	if !strings.Contains(stdout, "✗ http_req_failed rate<0.01") {
		t.Errorf("failing threshold not marked:\n%s", stdout)
	}
}

// TestRunReportNotCollected — never print zeros when nothing was collected:
// that would read as a passing test that measured nothing.
func TestRunReportNotCollected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"lt-2","name":"r","status":"RUNNING","version":1}`)
	}))
	defer srv.Close()
	code, stdout, stderr := exec(t, srv, "run", "report", "ns-1", "lt-2")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	if !strings.Contains(stderr, "not collected") {
		t.Errorf("stderr = %q, want an explicit not-collected note", stderr)
	}
	if strings.Contains(stdout, "http_reqs") || strings.Contains(stdout, "0.00%") {
		t.Errorf("stdout printed phantom metrics:\n%s", stdout)
	}
}

// TestRunReportCollectionFailure — the pod was gone; say why, not "0 requests".
func TestRunReportCollectionFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"id":"lt-3","name":"r","status":"COMPLETED","version":2,
			"result_collected_at":"2026-11-12T10:04:11Z",
			"result_parse_error":"summary unavailable: runner pod not found (already deleted?)"
		}`)
	}))
	defer srv.Close()
	code, _, stderr := exec(t, srv, "run", "report", "ns-1", "lt-3")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr, "runner pod not found") {
		t.Errorf("stderr = %q", stderr)
	}
}

// TestRunReportJSONIsTheRun — --json emits the server payload unchanged, so
// .result_summary is available to jq without a client-side re-shaping.
func TestRunReportJSONIsTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"lt-4","status":"COMPLETED","version":1,
			"result_summary":{"http_reqs":5,"rps":1,"p95_ms":2,"thresholds_failed":false}}`)
	}))
	defer srv.Close()
	code, stdout, stderr := exec(t, srv, "--json", "run", "report", "ns-1", "lt-4")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr %s)", code, stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	summary, ok := out["result_summary"].(map[string]any)
	if !ok {
		t.Fatalf("result_summary missing: %v", out)
	}
	if summary["http_reqs"] != float64(5) {
		t.Errorf("http_reqs = %v", summary["http_reqs"])
	}
}

// TestCommasAndMillis pins the presentation helpers.
func TestCommasAndMillis(t *testing.T) {
	for _, tc := range []struct {
		got  string
		want string
	}{
		{commas(0), "0"},
		{commas(999), "999"},
		{commas(12480), "12,480"},
		{commas(-1234567), "-1,234,567"},
		{millis(82), "82ms"},
		{millis(61.2), "61.20ms"},
		{millis(1500), "1.50s"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
