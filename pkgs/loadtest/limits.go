package loadtest

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Documented guardrail defaults (plans/42, docs/loadtest-manager-api.md §5).
// They are also the values used when a composition leaves Limits zero-valued,
// so the "fail open because nobody wired the knob" case cannot happen.
const (
	DefaultMaxVUs            = 500
	DefaultMaxDuration       = 30 * time.Minute
	DefaultMaxTimeoutMinutes = 60

	// minDuration rejects typo durations such as "1" (which time.ParseDuration
	// also rejects) and sub-second runs that cannot produce a usable summary.
	minDuration = 10 * time.Second
)

// Limits are the per-load-test ceilings the control plane enforces at create
// time. They are deployment configuration (MAX_VUS / MAX_DURATION /
// MAX_TIMEOUT_MINUTES), not per-namespace quotas: quota enforcement via
// tbl_reservation_ledger is a separate, deferred concern.
type Limits struct {
	MaxVUs            int
	MaxDuration       time.Duration
	MaxTimeoutMinutes int
}

// WithDefaults fills unset fields with the documented defaults.
func (l Limits) WithDefaults() Limits {
	if l.MaxVUs <= 0 {
		l.MaxVUs = DefaultMaxVUs
	}
	if l.MaxDuration <= 0 {
		l.MaxDuration = DefaultMaxDuration
	}
	if l.MaxTimeoutMinutes <= 0 {
		l.MaxTimeoutMinutes = DefaultMaxTimeoutMinutes
	}
	return l
}

// ValidateLoadConfig checks an already-defaulted load_config against the
// platform limits. It returns a platform error code plus a human-readable
// message; an empty code means "accepted".
//
// Validation runs before any DB write, so a rejected request never leaves a
// row behind and never reaches the Runner. Distinct codes (rather than the
// generic VALIDATION_FAILED) let clients print a specific remedy.
func ValidateLoadConfig(cfg map[string]any, limits Limits) (code, msg string) {
	l := limits.WithDefaults()

	vus, ok := strictInt(cfg["vusers"])
	if !ok || vus < 1 {
		return "VALIDATION_FAILED", "vusers must be a positive integer"
	}
	if vus > l.MaxVUs {
		return "VUS_LIMIT_EXCEEDED", fmt.Sprintf(
			"vusers %d exceeds the platform limit of %d", vus, l.MaxVUs)
	}

	rawDuration, _ := cfg["duration"].(string)
	if rawDuration == "" {
		rawDuration = "10m"
	}
	duration, err := time.ParseDuration(rawDuration)
	if err != nil {
		return "INVALID_DURATION", fmt.Sprintf(
			"duration %q is not a valid Go/k6 duration (examples: 30s, 10m, 1h30m)", rawDuration)
	}
	if duration < minDuration {
		return "VALIDATION_FAILED", fmt.Sprintf(
			"duration %s is too short: the minimum is %s", duration, minDuration)
	}
	if duration > l.MaxDuration {
		return "DURATION_LIMIT_EXCEEDED", fmt.Sprintf(
			"duration %s exceeds the platform limit of %s", duration, l.MaxDuration)
	}

	timeout, ok := strictInt(cfg["timeout_minutes"])
	if !ok || timeout < 1 {
		return "VALIDATION_FAILED", "timeout_minutes must be a positive integer"
	}
	if timeout > l.MaxTimeoutMinutes {
		return "TIMEOUT_LIMIT_EXCEEDED", fmt.Sprintf(
			"timeout_minutes %d exceeds the platform limit of %d", timeout, l.MaxTimeoutMinutes)
	}
	// The reaper transitions RUNNING → TIMED_OUT at timeout_at, so a timeout at
	// or below the run duration guarantees a healthy run is killed mid-flight.
	if time.Duration(timeout)*time.Minute <= duration {
		return "TIMEOUT_TOO_SHORT", fmt.Sprintf(
			"timeout_minutes %d must be greater than duration %s", timeout, duration)
	}
	return "", ""
}

// strictInt parses an integer from decoded JSON without silently substituting
// a default (intOf's fallback is right for the happy path, wrong for
// validation: it would turn "vusers": "many" into 200).
func strictInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	}
	return 0, false
}
