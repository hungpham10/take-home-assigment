package runner

import (
	"strconv"
	"strings"
)

// Summary is the parsed end-of-run k6 report. It is intentionally a small,
// version-tolerant subset of the metrics k6 prints: the fields a load-test
// consumer actually uses, parsed from the *text* summary.
//
// Why text and not `--summary-export`: the Kubernetes path can only read the
// runner pod log, so the local and K8s backends would need two different
// parsers (and two different failure modes) if they used different formats.
// One parser over the text tail keeps `runner.Status.Summary` identical for
// both backends (plans/45 §2).
type Summary struct {
	HTTPReqs         int         `json:"http_reqs"`
	RPS              float64     `json:"rps"`
	P95              float64     `json:"p95_ms"`
	P99              float64     `json:"p99_ms"`
	Avg              float64     `json:"avg_ms"`
	ErrorRate        float64     `json:"error_rate"` // 0..1
	Checks           int         `json:"checks"`
	ChecksFailed     int         `json:"checks_failed"`
	Thresholds       []Threshold `json:"thresholds,omitempty"`
	ThresholdsFailed bool        `json:"thresholds_failed"`
	// ParseError explains why the numbers above are partial or absent. An
	// empty ParseError means a complete summary was found.
	ParseError string `json:"parse_error,omitempty"`
}

// Threshold is one k6 threshold line from the `█ THRESHOLDS` block.
type Threshold struct {
	// Metric is the metric group the threshold belongs to
	// ("http_req_duration", "http_req_failed", …). Empty for the legacy
	// layout, which printed no grouping.
	Metric string `json:"metric,omitempty"`
	// Name is the threshold expression as written in the script, e.g.
	// "p(95)<200" or "rate<0.01".
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

// String renders one threshold the way a report reads it.
func (t Threshold) String() string {
	if t.Metric == "" {
		return t.Name
	}
	return t.Metric + " " + t.Name
}

// Empty reports whether nothing at all was measured (no requests, no checks,
// no thresholds). Callers use it to print "not collected" instead of zeros.
func (s Summary) Empty() bool {
	return s.HTTPReqs == 0 && s.Checks == 0 && s.P95 == 0 && s.Avg == 0 && len(s.Thresholds) == 0
}

// ParseSummary parses k6's text summary. It never panics and never returns an
// error: a run whose summary cannot be read is still a run, and the caller has
// to store its terminal status regardless. Problems are reported through
// Summary.ParseError, with whatever could be parsed kept.
//
// Accepted layouts:
//   - k6 v1.x: `█ THRESHOLDS` + `█ TOTAL RESULTS` blocks with `✓`/`✗` lines;
//   - k6 v0.4x: inline `✓ N ✗ M` counters and no summary boxes;
//   - anything else: ParseError set, zero values.
func ParseSummary(raw []byte) Summary {
	var s Summary
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		s.ParseError = "no k6 summary found (empty output)"
		return s
	}

	lines := strings.Split(text, "\n")
	haveBlock := strings.Contains(text, "█ TOTAL RESULTS") || strings.Contains(text, "█ THRESHOLDS")
	inThresholds := false
	thresholdGroup := ""
	sawMetrics := false
	sawRPS := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "█") {
			inThresholds = strings.Contains(trimmed, "THRESHOLDS")
			thresholdGroup = ""
			continue
		}
		// Threshold result line: ✓/✗ followed by a quoted expression. Legacy
		// check lines ("✓ status is 200") have no quotes and are ignored.
		if inThresholds && (strings.HasPrefix(trimmed, "✓") || strings.HasPrefix(trimmed, "✗")) &&
			strings.Contains(trimmed, "'") {
			name := between(trimmed, "'", "'")
			if name == "" {
				continue
			}
			ok := strings.HasPrefix(trimmed, "✓")
			s.Thresholds = append(s.Thresholds, Threshold{Metric: thresholdGroup, Name: name, OK: ok})
			if !ok {
				s.ThresholdsFailed = true
			}
			continue
		}
		idx := strings.LastIndex(trimmed, ":")
		if idx < 0 {
			continue
		}
		name := strings.Trim(strings.TrimSpace(trimmed[:idx]), ".")
		values := trimmed[idx+1:]
		switch name {
		case "http_reqs":
			sawMetrics = true
			s.HTTPReqs = int(firstNumber(values))
			if rps, ok := numberBefore(values, "/s"); ok {
				s.RPS = rps
				sawRPS = true
			}
		case "http_req_duration":
			// The aggregate line; `{ expected_response:true }` sub-lines have a
			// different name and are skipped naturally.
			if avg, ok := valueAfter(values, "avg="); ok {
				s.Avg = parseDuration(avg)
			}
			if p95, ok := valueAfter(values, "p(95)="); ok {
				s.P95 = parseDuration(p95)
			}
			if p99, ok := valueAfter(values, "p(99)="); ok {
				s.P99 = parseDuration(p99)
			}
		case "http_req_failed":
			if pct, ok := numberBefore(values, "%"); ok {
				s.ErrorRate = pct / 100
			}
		case "checks_total":
			s.Checks = int(firstNumber(values))
		case "checks_failed":
			// "100.00% 9137 out of 9137" → the count is after the percentage,
			// not the percentage itself.
			s.ChecksFailed = int(countAfterPercent(values))
		case "checks":
			// Legacy layout: "100.00% ✓ 12480  ✗ 0".
			passed, _ := countAfter(values, "✓")
			failed, _ := countAfter(values, "✗")
			s.Checks = int(passed + failed)
			s.ChecksFailed = int(failed)
		}
	}

	switch {
	case !sawMetrics && len(s.Thresholds) == 0 && !haveBlock:
		s.ParseError = "no k6 summary found (run produced no metrics)"
	case !sawMetrics:
		s.ParseError = "incomplete k6 summary (no http_reqs line)"
	case !sawRPS && s.HTTPReqs > 0:
		s.ParseError = "incomplete k6 summary (no requests/second figure)"
	case haveBlock && s.P95 == 0 && s.Avg == 0:
		s.ParseError = "incomplete k6 summary (no http_req_duration line)"
	}
	return s
}

// --- scanning helpers ------------------------------------------------------

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// numberAt parses the first number starting at or after i (skipping spaces).
func numberAt(s string, i int) (float64, bool) {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start := i
	for i < len(s) {
		c := s[i]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			i++
			continue
		}
		break
	}
	if start == i {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(s[start:i], "."), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func firstNumber(s string) float64 {
	v, _ := numberAt(s, 0)
	return v
}

func numberBefore(s, marker string) (float64, bool) {
	i := strings.Index(s, marker)
	if i < 0 {
		return 0, false
	}
	// Walk backwards over the number that immediately precedes the marker.
	// Spaces terminate it: "2     0.337781/s" must yield 0.337781, not "2     0.337781".
	j := i
	for j > 0 && isNumericByte(s[j-1]) {
		j--
	}
	if j == i {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(s[j:i], "."), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func isNumericByte(c byte) bool {
	return (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+'
}

// countAfterPercent returns the first number after the percent sign, falling
// back to the first number when the line has no percentage.
func countAfterPercent(s string) float64 {
	if i := strings.Index(s, "%"); i >= 0 {
		return firstNumber(s[i+1:])
	}
	return firstNumber(s)
}

func numberAfter(s, marker string) float64 {
	i := strings.Index(s, marker)
	if i < 0 {
		return 0
	}
	v, _ := numberAt(s, i+len(marker))
	return v
}

func countAfter(s, marker string) (float64, bool) {
	return numberAfter(s, marker), strings.Contains(s, marker)
}

// valueAfter returns the raw token after "key=" (e.g. "avg=7.93ms" → "7.93ms").
func valueAfter(s, key string) (string, bool) {
	i := strings.Index(s, key)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(key):]
	j := 0
	for j < len(rest) && rest[j] != ' ' && rest[j] != '\t' {
		j++
	}
	if j == 0 {
		return "", false
	}
	return rest[:j], true
}

// parseDuration converts a k6 duration string to milliseconds. Bare numbers
// are already milliseconds (k6 prints ms for duration metrics).
func parseDuration(s string) float64 {
	s = strings.TrimSpace(s)
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "ns"):
		mult, s = 1e-6, strings.TrimSuffix(s, "ns")
	case strings.HasSuffix(s, "µs"):
		mult, s = 1e-3, strings.TrimSuffix(s, "µs")
	case strings.HasSuffix(s, "us"):
		mult, s = 1e-3, strings.TrimSuffix(s, "us")
	case strings.HasSuffix(s, "ms"):
		mult, s = 1, strings.TrimSuffix(s, "ms")
	case strings.HasSuffix(s, "s"):
		mult, s = 1e3, strings.TrimSuffix(s, "s")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v * mult
}
