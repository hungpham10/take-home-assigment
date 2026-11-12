package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeK6 writes an executable that ignores its arguments (so Local's
// `run --vus … --quiet <script>` invocation works) and prints a k6-shaped
// summary, optionally failing.
func fakeK6(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "k6")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const summaryFixture = `
cat <<'EOF'
  █ TOTAL RESULTS

    checks_total.......: 4       2.0/s
    checks_succeeded...: 100.00% 4 out of 4
    checks_failed......: 0.00%   0 out of 4

    ✓ status is 200

    HTTP
    http_req_duration..............: avg=12ms   min=1ms   med=11ms  max=40ms  p(90)=20ms  p(95)=25ms
    http_req_failed................: 0.00% 0 out of 4
    http_reqs......................: 4     2/s
EOF
`

// waitTerminal polls Status until a terminal state (or the deadline).
func waitTerminal(t *testing.T, l *Local, ref string) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := l.Status(context.Background(), ref)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.State != "RUNNING" && st.State != "PROVISIONING" {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never reached a terminal state (last %s)", st.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLocalStatusCarriesSummary — the summary must be captured before the proc
// is removed from the table (plans/45 §1).
func TestLocalStatusCarriesSummary(t *testing.T) {
	l := NewLocal(fakeK6(t, summaryFixture))
	ctx := context.Background()
	ref, err := l.Provision(ctx, Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Start(ctx, ref, Spec{Script: []byte("export default function(){}"), Duration: "1s", VUs: 1}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitTerminal(t, l, ref)
	if st.State != "COMPLETED" {
		t.Fatalf("state = %s (%s)", st.State, st.Msg)
	}
	if len(st.Summary) == 0 {
		t.Fatal("Summary is empty: the stdout tail was lost")
	}
	s := ParseSummary(st.Summary)
	if s.ParseError != "" {
		t.Errorf("summary did not parse: %s\n%s", s.ParseError, st.Summary)
	}
	if s.HTTPReqs != 4 || s.Checks != 4 {
		t.Errorf("parsed summary = %+v", s)
	}
	if !strings.Contains(string(st.Summary), "TOTAL RESULTS") {
		t.Errorf("Summary = %q", st.Summary)
	}
	// A second poll must not panic or re-read a removed proc.
	if st2, err := l.Status(ctx, ref); err != nil || !st2.Missing {
		t.Errorf("post-terminal status = (%+v, %v), want Missing", st2, err)
	}
}

// TestLocalFailingRunStillCarriesSummary — a threshold violation exits non-zero,
// and that is exactly when the summary matters most.
func TestLocalFailingRunStillCarriesSummary(t *testing.T) {
	l := NewLocal(fakeK6(t, summaryFixture+`exit 99`))
	ctx := context.Background()
	ref, _ := l.Provision(ctx, Spec{})
	if err := l.Start(ctx, ref, Spec{Script: []byte("x"), Duration: "1s", VUs: 1}); err != nil {
		t.Fatal(err)
	}
	st := waitTerminal(t, l, ref)
	if st.State != "FAILED" {
		t.Fatalf("state = %s, want FAILED (k6 exit 99 = thresholds failed)", st.State)
	}
	if len(st.Summary) == 0 {
		t.Fatal("Summary is empty for a threshold-failing run")
	}
	if s := ParseSummary(st.Summary); s.HTTPReqs != 4 {
		t.Errorf("parsed summary = %+v", s)
	}
}

// TestLocalEmptyOutputIsNotAnError — a runner that prints nothing still ends
// the run; the reconciler records a parse error.
func TestLocalEmptyOutputIsNotAnError(t *testing.T) {
	l := NewLocal(fakeK6(t, "exit 0"))
	ctx := context.Background()
	ref, _ := l.Provision(ctx, Spec{})
	if err := l.Start(ctx, ref, Spec{Script: []byte("x"), Duration: "1s", VUs: 1}); err != nil {
		t.Fatal(err)
	}
	st := waitTerminal(t, l, ref)
	if st.State != "COMPLETED" {
		t.Fatalf("state = %s", st.State)
	}
	if len(st.Summary) != 0 {
		t.Errorf("Summary = %q, want empty", st.Summary)
	}
	if st.SummaryErr != "" {
		t.Errorf("SummaryErr = %q, want empty (an empty log is not a retrieval failure)", st.SummaryErr)
	}
}
