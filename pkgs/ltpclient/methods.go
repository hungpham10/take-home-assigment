package ltpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// apiPrefix is the versioned BFF root. Every path below is relative to it, and
// the client tests assert the built paths, so a server route change fails here.
const apiPrefix = "/api/loadtest/v1"

// --- namespaces ------------------------------------------------------------

// CreateNamespace creates a namespace (Idempotency-Key is sent for you).
//
// The server has two success shapes for this endpoint:
//   - name-only create → 201 with the namespace object;
//   - create-with-spec → 201/202 with {namespace, services, databases, ingress}.
//
// Both are accepted here so the CLI does not have to know which one it called.
func (c *Client) CreateNamespace(ctx context.Context, req CreateNamespaceRequest) (*Namespace, error) {
	res, err := c.CreateNamespaceWithSpec(ctx, req)
	if err != nil {
		return nil, err
	}
	return &res.Namespace, nil
}

// CreateNamespaceWithSpec is CreateNamespace for callers that also want the
// generated component ids (useful for "created 1 service, 1 database" output).
func (c *Client) CreateNamespaceWithSpec(ctx context.Context, req CreateNamespaceRequest) (*CreateNamespaceResult, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, apiPrefix+"/namespaces", req, &raw, true); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("create namespace: empty response body")
	}
	// Nested shape (create-with-spec).
	var nested struct {
		Namespace *Namespace `json:"namespace"`
		Status    string     `json:"status"`
		Services  []string   `json:"services"`
		Databases []string   `json:"databases"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, fmt.Errorf("create namespace: decode response: %w", err)
	}
	if nested.Namespace != nil {
		return &CreateNamespaceResult{
			Namespace: *nested.Namespace,
			Status:    nested.Status,
			Services:  nested.Services,
			Databases: nested.Databases,
		}, nil
	}
	// Flat shape (name-only create).
	var flat Namespace
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("create namespace: decode response: %w", err)
	}
	if flat.ID == "" {
		return nil, fmt.Errorf("create namespace: response has no namespace id: %.200s", raw)
	}
	return &CreateNamespaceResult{Namespace: flat, Status: flat.Status}, nil
}

// GetNamespace fetches one namespace.
func (c *Client) GetNamespace(ctx context.Context, id string) (*Namespace, error) {
	var ns Namespace
	if err := c.do(ctx, http.MethodGet, pathf("/namespaces/%s", id), nil, &ns, false); err != nil {
		return nil, err
	}
	return &ns, nil
}

// ListNamespaces returns the caller's namespaces.
func (c *Client) ListNamespaces(ctx context.Context) ([]Namespace, error) {
	var out struct {
		Namespaces []Namespace `json:"namespaces"`
	}
	if err := c.do(ctx, http.MethodGet, apiPrefix+"/namespaces", nil, &out, false); err != nil {
		return nil, err
	}
	return out.Namespaces, nil
}

// ValidateNamespace runs the readiness gates synchronously (422 on failure).
func (c *Client) ValidateNamespace(ctx context.Context, id string) (*Readiness, error) {
	var r Readiness
	if err := c.do(ctx, http.MethodPost, pathf("/namespaces/%s/validate", id), nil, &r, false); err != nil {
		// A 422 body is a readiness report, not an error envelope; surface it
		// as an error because validation did not pass.
		return nil, err
	}
	return &r, nil
}

// ApplyNamespace starts (K8S: 202 APPLYING) or completes (legacy: 200 READY) apply.
func (c *Client) ApplyNamespace(ctx context.Context, id string) (*ApplyResult, error) {
	var res ApplyResult
	if err := c.do(ctx, http.MethodPost, pathf("/namespaces/%s/apply", id), nil, &res, true); err != nil {
		return nil, err
	}
	return &res, nil
}

// Readiness fetches the component checklist without mutating anything.
func (c *Client) Readiness(ctx context.Context, id string) (*Readiness, error) {
	var r Readiness
	if err := c.do(ctx, http.MethodGet, pathf("/namespaces/%s/readiness", id), nil, &r, false); err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteNamespace moves the namespace to DELETING (202).
func (c *Client) DeleteNamespace(ctx context.Context, id string) error {
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	return c.do(ctx, http.MethodDelete, pathf("/namespaces/%s", id), nil, &out, true)
}

// --- scripts ---------------------------------------------------------------

// UploadScript stages k6 script content (≤1 MiB, enforced client-side too).
func (c *Client) UploadScript(ctx context.Context, nsID, name, content string) (*Script, error) {
	body := struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}{Name: name, Content: content}
	var sc Script
	if err := c.do(ctx, http.MethodPost, pathf("/namespaces/%s/scripts", nsID), body, &sc, true); err != nil {
		return nil, err
	}
	return &sc, nil
}

// GetScript fetches one script (status drives the validation gate).
func (c *Client) GetScript(ctx context.Context, nsID, scriptID string) (*Script, error) {
	var sc Script
	if err := c.do(ctx, http.MethodGet, pathf("/namespaces/%s/scripts/%s", nsID, scriptID), nil, &sc, false); err != nil {
		return nil, err
	}
	return &sc, nil
}

// --- load tests ------------------------------------------------------------

// CreateLoadTest creates and triggers a run (201, status PROVISIONING/QUEUED).
func (c *Client) CreateLoadTest(ctx context.Context, nsID string, req CreateLoadTestRequest) (*LoadTest, error) {
	var lt LoadTest
	if err := c.do(ctx, http.MethodPost, pathf("/namespaces/%s/loadtests", nsID), req, &lt, true); err != nil {
		return nil, err
	}
	return &lt, nil
}

// GetLoadTest fetches one run.
func (c *Client) GetLoadTest(ctx context.Context, nsID, ltID string) (*LoadTest, error) {
	var lt LoadTest
	if err := c.do(ctx, http.MethodGet, pathf("/namespaces/%s/loadtests/%s", nsID, ltID), nil, &lt, false); err != nil {
		return nil, err
	}
	return &lt, nil
}

// ListLoadTests returns a namespace's runs, newest first.
func (c *Client) ListLoadTests(ctx context.Context, nsID string) ([]LoadTest, error) {
	var out struct {
		LoadTests []LoadTest `json:"load_tests"`
	}
	if err := c.do(ctx, http.MethodGet, pathf("/namespaces/%s/loadtests", nsID), nil, &out, false); err != nil {
		return nil, err
	}
	return out.LoadTests, nil
}

// StopLoadTest requests a stop (202; the reconciler finishes STOPPING→STOPPED).
func (c *Client) StopLoadTest(ctx context.Context, nsID, ltID string) error {
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	return c.do(ctx, http.MethodPost, pathf("/namespaces/%s/loadtests/%s/stop", nsID, ltID), nil, &out, true)
}

// Events returns the run's transition trail.
func (c *Client) Events(ctx context.Context, nsID, ltID string) ([]Event, error) {
	var out struct {
		Events []Event `json:"events"`
	}
	if err := c.do(ctx, http.MethodGet, pathf("/namespaces/%s/loadtests/%s/events", nsID, ltID), nil, &out, false); err != nil {
		return nil, err
	}
	return out.Events, nil
}

// --- polling ---------------------------------------------------------------

// Terminal statuses for a load test (schema CHECK constraint in
// sql/0004_script_loadtest.sql).
func isTerminalRunStatus(s string) bool {
	switch s {
	case "COMPLETED", "FAILED", "STOPPED", "TIMED_OUT", "TIMEOUT_CLEANED", "CLEANUP_FAILED":
		return true
	}
	return false
}

// WaitForReady polls /readiness until Ready or the deadline. It returns the
// last report observed (useful for printing blocking_reasons on timeout).
func (c *Client) WaitForReady(ctx context.Context, nsID string, every time.Duration, deadline time.Duration) (*Readiness, error) {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var last *Readiness
	for {
		r, err := c.Readiness(ctx, nsID)
		if err != nil {
			return last, err
		}
		last = r
		if r.Ready || r.ReadyForLoadtest {
			return r, nil
		}
		if err := sleepCtx(ctx, every); err != nil {
			return last, fmt.Errorf("namespace not ready after %s (status %s)", deadline, r.Status)
		}
	}
}

// WaitForScriptValidated polls a script until it reaches a terminal validation
// status. Returns the script and whether it is VALIDATED.
func (c *Client) WaitForScriptValidated(ctx context.Context, nsID, scriptID string, every, deadline time.Duration) (*Script, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		sc, err := c.GetScript(ctx, nsID, scriptID)
		if err != nil {
			return nil, false, err
		}
		switch sc.Status {
		case "VALIDATED":
			return sc, true, nil
		case "INVALID", "VALIDATION_FAILED":
			return sc, false, nil
		}
		if err := sleepCtx(ctx, every); err != nil {
			return sc, false, fmt.Errorf("script %s still %s after %s", sc.ID, sc.Status, deadline)
		}
	}
}

// WaitForRun polls a load test until it reaches a terminal status.
func (c *Client) WaitForRun(ctx context.Context, nsID, ltID string, every, deadline time.Duration) (*LoadTest, error) {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var last *LoadTest
	for {
		lt, err := c.GetLoadTest(ctx, nsID, ltID)
		if err != nil {
			return last, err
		}
		last = lt
		if isTerminalRunStatus(lt.Status) {
			return lt, nil
		}
		if err := sleepCtx(ctx, every); err != nil {
			return last, fmt.Errorf("run %s still %s after %s", lt.ID, lt.Status, deadline)
		}
	}
}

// sleepCtx is a context-aware sleep (package-level so tests can shorten it).
var sleepCtx = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pathf builds a versioned path with escaped path segments.
func pathf(format string, args ...any) string {
	escaped := make([]any, len(args))
	for i, a := range args {
		escaped[i] = url.PathEscape(fmt.Sprint(a))
	}
	return apiPrefix + fmt.Sprintf(format, escaped...)
}
