package runner

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// approx compares floats with a tolerance that is irrelevant for report values.
func approx(t *testing.T, label string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v (±%v)", label, got, want, tol)
	}
}

// TestParseSummaryPass is the primary contract: a green k6 v1.x summary.
func TestParseSummaryPass(t *testing.T) {
	s := ParseSummary(readFixture(t, "pass.txt"))
	if s.ParseError != "" {
		t.Fatalf("ParseError = %q", s.ParseError)
	}
	if s.HTTPReqs != 2 {
		t.Errorf("HTTPReqs = %d, want 2", s.HTTPReqs)
	}
	approx(t, "RPS", s.RPS, 0.337781, 1e-6)
	approx(t, "P95", s.P95, 7.93, 1e-9)
	approx(t, "Avg", s.Avg, 7.93, 1e-9)
	approx(t, "ErrorRate", s.ErrorRate, 0, 1e-12)
	if s.Checks != 2 || s.ChecksFailed != 0 {
		t.Errorf("checks = %d/%d, want 0/2 failed", s.ChecksFailed, s.Checks)
	}
	if s.ThresholdsFailed {
		t.Errorf("ThresholdsFailed = true, thresholds = %+v", s.Thresholds)
	}
	if len(s.Thresholds) != 2 {
		t.Fatalf("thresholds = %+v, want 2", s.Thresholds)
	}
	for _, th := range s.Thresholds {
		if !th.OK {
			t.Errorf("threshold %q marked failed", th.Name)
		}
	}
	if s.Thresholds[0].Name != "p(95)<2000" || s.Thresholds[1].Name != "rate<0.01" {
		t.Errorf("threshold names = %+v", s.Thresholds)
	}
}

// TestParseSummaryFail — one ✗ flips ThresholdsFailed and keeps the other.
func TestParseSummaryFail(t *testing.T) {
	s := ParseSummary(readFixture(t, "fail.txt"))
	if s.ParseError != "" {
		t.Fatalf("ParseError = %q", s.ParseError)
	}
	if !s.ThresholdsFailed {
		t.Fatal("ThresholdsFailed = false with a ✗ threshold")
	}
	var failed, passed int
	for _, th := range s.Thresholds {
		if th.OK {
			passed++
		} else {
			failed++
		}
	}
	if passed != 1 || failed != 1 {
		t.Errorf("thresholds = %+v", s.Thresholds)
	}
	approx(t, "ErrorRate", s.ErrorRate, 1.0, 1e-9)
	if s.HTTPReqs < 100 {
		t.Errorf("HTTPReqs = %d, want many (the failing run issued thousands)", s.HTTPReqs)
	}
	if s.ChecksFailed == 0 || s.ChecksFailed != s.Checks {
		t.Errorf("checks failed/total = %d/%d, want all failed", s.ChecksFailed, s.Checks)
	}
}

// TestParseSummaryLegacyLayout — k6 0.4x printed ✓/✗ counts inline and had no
// THRESHOLDS/TOTAL RESULTS boxes. Numbers must still parse.
func TestParseSummaryLegacyLayout(t *testing.T) {
	s := ParseSummary(readFixture(t, "v0_49.txt"))
	if s.ParseError != "" {
		t.Fatalf("ParseError = %q", s.ParseError)
	}
	if s.HTTPReqs != 12480 {
		t.Errorf("HTTPReqs = %d, want 12480", s.HTTPReqs)
	}
	approx(t, "RPS", s.RPS, 41.6, 1e-9)
	approx(t, "P95", s.P95, 82, 1e-9)
	approx(t, "Avg", s.Avg, 61.2, 1e-9)
	approx(t, "ErrorRate", s.ErrorRate, 0.0031, 1e-9)
	if s.Checks != 12480 || s.ChecksFailed != 0 {
		t.Errorf("checks = %d failed of %d", s.ChecksFailed, s.Checks)
	}
	// No threshold block in the legacy format: absence is not a failure.
	if s.ThresholdsFailed || len(s.Thresholds) != 0 {
		t.Errorf("thresholds = %+v failed=%v", s.Thresholds, s.ThresholdsFailed)
	}
}

// TestParseSummaryTruncated — the summary block started but the tail was cut.
// What is complete must be kept and the error must say so.
func TestParseSummaryTruncated(t *testing.T) {
	s := ParseSummary(readFixture(t, "truncated.txt"))
	if s.ParseError == "" {
		t.Fatal("ParseError is empty for a truncated summary")
	}
	if !strings.Contains(s.ParseError, "incomplete") {
		t.Errorf("ParseError = %q, want it to mention incompleteness", s.ParseError)
	}
	if s.Checks != 2 || s.ChecksFailed != 0 {
		t.Errorf("partial parse lost the checks block: %+v", s)
	}
	if s.HTTPReqs != 0 {
		t.Errorf("HTTPReqs = %d from a truncated fixture", s.HTTPReqs)
	}
}

// TestParseSummaryNoSummary — progress output only (crash before the summary).
func TestParseSummaryNoSummary(t *testing.T) {
	for _, name := range []string{"progress-only.txt", "empty.txt"} {
		s := ParseSummary(readFixture(t, name))
		if s.ParseError == "" {
			t.Errorf("%s: ParseError is empty", name)
		}
		if len(s.Thresholds) != 0 || s.HTTPReqs != 0 {
			t.Errorf("%s: parsed phantom values: %+v", name, s)
		}
		if s.ThresholdsFailed {
			t.Errorf("%s: ThresholdsFailed = true without thresholds", name)
		}
	}
}

// TestParseSummaryNeverPanics covers binary garbage and nil input.
func TestParseSummaryNeverPanics(_ *testing.T) {
	for _, raw := range [][]byte{
		nil,
		{},
		[]byte("\x00\x01\x02"),
		[]byte("█ TOTAL RESULTS\n    http_reqs.: not-a-number  also-not"),
		[]byte("█ THRESHOLDS\n\n    ✓ 'p(95)<200'\n"),
		[]byte("http_req_duration..............: avg= p(95)= p(99)= "),
	} {
		_ = ParseSummary(raw) // must not panic
	}
}

// TestParseSummaryUnits — duration unit conversion is where a naive parser
// silently reports microseconds as milliseconds.
func TestParseSummaryUnits(t *testing.T) {
	cases := []struct {
		value string
		want  float64 // milliseconds
	}{
		{"500µs", 0.5},
		{"500us", 0.5},
		{"7.93ms", 7.93},
		{"1.5s", 1500},
		{"0s", 0},
		{"123", 123}, // bare numbers are milliseconds (k6 prints ms for durations)
	}
	for _, tc := range cases {
		if got := parseDuration(tc.value); got != tc.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
	// A malformed duration must degrade to 0, never NaN.
	if got := parseDuration("abc"); got != 0 {
		t.Errorf("parseDuration(abc) = %v, want 0", got)
	}
}

// TestSummaryRedacted is the CLI/API contract helper: no numbers means the
// report says "not collected" instead of printing zeros.
func TestSummaryRedacted(t *testing.T) {
	if !(Summary{}).Empty() {
		t.Error("zero Summary must be Empty()")
	}
	if (Summary{HTTPReqs: 1}).Empty() {
		t.Error("Summary with requests must not be Empty()")
	}
	if !(Summary{ParseError: "boom"}).Empty() {
		t.Error("a Summary with only a ParseError measured nothing, so it must be Empty()")
	}
}
