// Package ltpclient is the typed Go client for the load-testing platform BFF
// (/api/loadtest/v1). It is the reusable half of cmds/ltp: the CLI owns flag
// parsing and presentation, this package owns HTTP.
//
// Design rules:
//   - no CLI types, no printing, no os.Exit;
//   - server error codes are surfaced verbatim (a new server code must not
//     require a client release);
//   - mutating calls always send an Idempotency-Key (the server requires it).
package ltpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultTimeout bounds one HTTP round-trip (not the polling helpers).
const DefaultTimeout = 30 * time.Second

// Client talks to the control-plane BFF.
type Client struct {
	// BaseURL is the API root, e.g. http://localhost:8080 (no trailing slash).
	BaseURL string
	// Token is the abt_ automation token; empty means header-based dev auth.
	Token string
	// TenantID/UserID/Role enable the development auth path (the headers nginx
	// normally injects after OIDC). Leave them empty when Token is set.
	TenantID string
	UserID   string
	Role     string
	// HTTP is optional; DefaultHTTP is used when nil.
	HTTP *http.Client
	// NewKey overrides idempotency-key generation (tests).
	NewKey func() string
}

// DefaultHTTP returns a client with the standard timeout.
func DefaultHTTP() *http.Client { return &http.Client{Timeout: DefaultTimeout} }

// Error is a platform error envelope. Code is the server's stable code
// (NAMESPACE_NOT_READY, VUS_LIMIT_EXCEEDED, …).
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
	}
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

// IsStateFailure reports whether the error is a precondition the caller can
// fix (409/422) rather than a transport/server fault. The CLI maps it to exit 3.
func IsStateFailure(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == http.StatusConflict || e.Status == http.StatusUnprocessableEntity
}

// --- types -----------------------------------------------------------------

// Namespace is the subset of tbl_namespaces the CLI renders.
type Namespace struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Slug             string    `json:"slug,omitempty"`
	Status           string    `json:"status"`
	ReadyForLoadtest bool      `json:"ready_for_loadtest"`
	Version          int       `json:"version"`
	CreatedAt        time.Time `json:"created_at"`
}

// Readiness is the /readiness payload (projection-backed in K8S mode).
type Readiness struct {
	NamespaceID      string         `json:"namespace_id"`
	Status           string         `json:"status"`
	ReadyForLoadtest bool           `json:"ready_for_loadtest"`
	Ready            bool           `json:"ready"`
	BlockingReasons  []string       `json:"blocking_reasons"`
	Components       map[string]any `json:"components"`
}

// Script is the subset of tbl_scripts the CLI renders.
type Script struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Version  int    `json:"version"`
	SizeByte int    `json:"size_bytes"`
}

// LoadTest is the run row the CLI renders.
type LoadTest struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	ScriptID   string          `json:"script_id"`
	RunnerType string          `json:"runner_type"`
	RunnerRef  *string         `json:"runner_ref"`
	LoadConfig json.RawMessage `json:"load_config"`
	Targets    json.RawMessage `json:"targets"`
	StartedAt  *time.Time      `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	Unreliable bool            `json:"unreliable,omitempty"`
	// Results (plans/45). All four are absent until the reconciler collects
	// the k6 summary; ResultCollectedAt being set with no summary means
	// "collection ran and there was nothing to report" (see ResultParseError).
	ResultSummary     *RunSummary `json:"result_summary,omitempty"`
	ThresholdsFailed  *bool       `json:"thresholds_failed,omitempty"`
	ResultCollectedAt *time.Time  `json:"result_collected_at,omitempty"`
	ResultParseError  *string     `json:"result_parse_error,omitempty"`
	Version           int         `json:"version"`
}

// RunSummary mirrors the server's parsed k6 summary (pkgs/runner.Summary).
// It is redeclared here rather than imported so the CLI client stays free of
// the execution backends (pkgs/runner pulls in the Kubernetes client).
type RunSummary struct {
	HTTPReqs         int            `json:"http_reqs"`
	RPS              float64        `json:"rps"`
	P95              float64        `json:"p95_ms"`
	P99              float64        `json:"p99_ms"`
	Avg              float64        `json:"avg_ms"`
	ErrorRate        float64        `json:"error_rate"`
	Checks           int            `json:"checks"`
	ChecksFailed     int            `json:"checks_failed"`
	Thresholds       []RunThreshold `json:"thresholds,omitempty"`
	ThresholdsFailed bool           `json:"thresholds_failed"`
	ParseError       string         `json:"parse_error,omitempty"`
}

// RunThreshold is one k6 threshold result.
type RunThreshold struct {
	Metric string `json:"metric,omitempty"`
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
}

// String renders the threshold the way the report prints it.
func (t RunThreshold) String() string {
	if t.Metric == "" {
		return t.Name
	}
	return t.Metric + " " + t.Name
}

// Event is one lifecycle transition.
type Event struct {
	ToStatus  string    `json:"to_status"`
	Reason    *string   `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// ApplyResult is the 202/200 body of POST /apply. K8S mode returns 202 with
// status APPLYING; legacy returns 200 READY.
type ApplyResult struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	DesiredGeneration int    `json:"desired_generation,omitempty"`
}

// CreateNamespaceRequest is the create-with-spec body (spec is passed through
// unmodified so the CLI does not need to model the component schema).
type CreateNamespaceRequest struct {
	Name        string          `json:"name"`
	Slug        string          `json:"slug,omitempty"`
	Description string          `json:"description,omitempty"`
	Spec        json.RawMessage `json:"spec,omitempty"`
}

// CreateNamespaceResult is the create-with-spec response: the namespace plus
// the component ids the server generated for the spec.
type CreateNamespaceResult struct {
	Namespace Namespace `json:"namespace"`
	// Status is set when the create also applied (apply:true in the spec).
	Status    string   `json:"status,omitempty"`
	Services  []string `json:"services,omitempty"`
	Databases []string `json:"databases,omitempty"`
}

// CreateLoadTestRequest is POST /loadtests.
type CreateLoadTestRequest struct {
	Name       string         `json:"name"`
	ScriptID   string         `json:"script_id"`
	LoadConfig map[string]any `json:"load_config,omitempty"`
}

// --- transport -------------------------------------------------------------

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return DefaultHTTP()
}

func (c *Client) base() string {
	return strings.TrimRight(c.BaseURL, "/")
}

// do performs one request. out may be nil. mutate adds the idempotency key.
func (c *Client) do(ctx context.Context, method, path string, body, out any, mutate bool) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.TenantID != "" {
		req.Header.Set("X-Tenant-Id", c.TenantID)
	}
	if c.UserID != "" {
		req.Header.Set("X-User-Id", c.UserID)
	}
	if c.Role != "" {
		req.Header.Set("X-User-Role", c.Role)
	}
	if mutate {
		req.Header.Set("Idempotency-Key", c.newKey())
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		return decodeError(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w (body: %.200s)", method, path, err, raw)
	}
	return nil
}

func (c *Client) newKey() string {
	if c.NewKey != nil {
		return c.NewKey()
	}
	return uuid.NewString()
}

// errorEnvelope covers both shapes the server emits:
// {"error":{"code","message"}} (httpx) and flat {"error":"...","reason":"..."}.
type errorEnvelope struct {
	Error   json.RawMessage `json:"error"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Reason  string          `json:"reason"`
}

func decodeError(status int, raw []byte) error {
	var env errorEnvelope
	_ = json.Unmarshal(raw, &env)
	e := &Error{Status: status, Code: env.Code, Message: env.Message}
	if len(env.Error) > 0 {
		var nested struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(env.Error, &nested); err == nil && nested.Code != "" {
			e.Code, e.Message = nested.Code, nested.Message
		} else {
			var flat string
			if err := json.Unmarshal(env.Error, &flat); err == nil && flat != "" {
				e.Message = flat
			}
		}
	}
	if e.Code == "" {
		e.Code = fmt.Sprintf("HTTP_%d", status)
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(raw))
	}
	if env.Reason != "" {
		e.Message = e.Message + " (" + env.Reason + ")"
	}
	return e
}
