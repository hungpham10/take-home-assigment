package ltpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/ltpclient"
)

// newTestClient starts a server whose behaviour is provided per test and
// returns a client pointed at it.
func newTestClient(t *testing.T, h http.HandlerFunc) (*ltpclient.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := &ltpclient.Client{
		BaseURL: srv.URL,
		Token:   "abt_test",
		HTTP:    srv.Client(),
		NewKey:  func() string { return "idem-test" },
	}
	return c, srv
}

func TestCreateNamespaceRequestShape(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotKey, gotCT string
	var gotBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth, gotKey, gotCT = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ns-1","name":"demo","status":"DRAFT","version":1}`))
	})

	ns, err := c.CreateNamespace(context.Background(), ltpclient.CreateNamespaceRequest{Name: "demo", Slug: "demo"})
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if ns.ID != "ns-1" || ns.Status != "DRAFT" {
		t.Errorf("namespace = %+v", ns)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/loadtest/v1/namespaces" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer abt_test" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotKey != "idem-test" {
		t.Errorf("idempotency-key = %q (mutations must send one)", gotKey)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotBody["name"] != "demo" || gotBody["slug"] != "demo" {
		t.Errorf("body = %v", gotBody)
	}
}

// TestNoIdempotencyKeyOnReads — GET must be replay-safe without a key.
func TestNoIdempotencyKeyOnReads(t *testing.T) {
	var key string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		_, _ = w.Write([]byte(`{"id":"ns-1","status":"READY"}`))
	})
	if _, err := c.GetNamespace(context.Background(), "ns-1"); err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if key != "" {
		t.Errorf("GET sent Idempotency-Key %q", key)
	}
}

func TestErrorCodesSurfaceVerbatim(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantMsgSub string
	}{
		{
			name: "401 envelope", status: http.StatusUnauthorized,
			body:     `{"error":{"code":"UNAUTHORIZED","message":"missing identity"}}`,
			wantCode: "UNAUTHORIZED", wantMsgSub: "missing identity",
		},
		{
			name: "409 not ready", status: http.StatusConflict,
			body:     `{"error":{"code":"NAMESPACE_NOT_READY","message":"namespace is not READY (status DRAFT)"}}`,
			wantCode: "NAMESPACE_NOT_READY", wantMsgSub: "DRAFT",
		},
		{
			name: "422 vus cap", status: http.StatusUnprocessableEntity,
			body:     `{"error":{"code":"VUS_LIMIT_EXCEEDED","message":"vusers 501 exceeds the platform limit of 500"}}`,
			wantCode: "VUS_LIMIT_EXCEEDED", wantMsgSub: "501",
		},
		{
			// Flat shape from the STATUS_STALE gate (no nested envelope).
			name: "409 flat + reason", status: http.StatusConflict,
			body:     `{"error":{"code":"NAMESPACE_NOT_READY","message":"status heartbeat stale"},"reason":"STATUS_STALE"}`,
			wantCode: "NAMESPACE_NOT_READY", wantMsgSub: "STATUS_STALE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.GetNamespace(context.Background(), "ns-1")
			var apiErr *ltpclient.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("error type = %T (%v)", err, err)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", apiErr.Code, tc.wantCode)
			}
			if !strings.Contains(apiErr.Error(), tc.wantMsgSub) {
				t.Errorf("message %q missing %q", apiErr.Error(), tc.wantMsgSub)
			}
			if !ltpclient.IsStateFailure(err) && tc.status != http.StatusUnauthorized {
				t.Errorf("IsStateFailure(%d) = false", tc.status)
			}
		})
	}
}

// TestNonJSONErrorBody — an nginx HTML 502 must still produce a usable error.
func TestNonJSONErrorBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	})
	_, err := c.GetNamespace(context.Background(), "ns-1")
	var apiErr *ltpclient.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T", err)
	}
	if apiErr.Status != http.StatusBadGateway || apiErr.Code != "HTTP_502" {
		t.Errorf("error = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Message, "502") {
		t.Errorf("message lost the body: %q", apiErr.Message)
	}
	if ltpclient.IsStateFailure(err) {
		t.Error("502 must not be a state failure (that is an outage, not a precondition)")
	}
}

// TestApplyAcceptedPreserved — K8S apply is 202 + desired_generation.
func TestApplyAcceptedPreserved(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/loadtest/v1/namespaces/ns-1/apply" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"ns-1","status":"APPLYING","desired_generation":3}`))
	})
	res, err := c.ApplyNamespace(context.Background(), "ns-1")
	if err != nil {
		t.Fatalf("ApplyNamespace: %v", err)
	}
	if res.Status != "APPLYING" || res.DesiredGeneration != 3 {
		t.Errorf("apply result = %+v", res)
	}
}

func TestWaitForReady(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			_, _ = w.Write([]byte(`{"namespace_id":"ns-1","status":"APPLYING","ready":false,"blocking_reasons":["waiting for Deployment"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"namespace_id":"ns-1","status":"READY","ready":true,"ready_for_loadtest":true}`))
	})
	r, err := c.WaitForReady(context.Background(), "ns-1", time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatalf("WaitForReady: %v", err)
	}
	if !r.Ready || r.Status != "READY" {
		t.Errorf("readiness = %+v", r)
	}
	if calls != 3 {
		t.Errorf("polls = %d, want 3", calls)
	}
}

// TestWaitForReadyTimeoutKeepsLastReport — the CLI prints blocking_reasons on
// timeout, so the last report must not be lost.
func TestWaitForReadyTimeoutKeepsLastReport(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"namespace_id":"ns-1","status":"DEGRADED","ready":false,"blocking_reasons":["demo-db: CrashLoopBackOff"]}`))
	})
	last, err := c.WaitForReady(context.Background(), "ns-1", time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if last == nil || len(last.BlockingReasons) == 0 || last.BlockingReasons[0] != "demo-db: CrashLoopBackOff" {
		t.Fatalf("last report = %+v", last)
	}
}

func TestWaitForScriptValidated(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = w.Write([]byte(`{"id":"sc-1","status":"VALIDATING"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"sc-1","status":"VALIDATED"}`))
	})
	sc, ok, err := c.WaitForScriptValidated(context.Background(), "ns-1", "sc-1", time.Millisecond, time.Second)
	if err != nil || !ok || sc.Status != "VALIDATED" {
		t.Fatalf("script = %+v ok=%v err=%v", sc, ok, err)
	}

	// INVALID is terminal and must not loop until the deadline.
	c2, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sc-2","status":"INVALID"}`))
	})
	sc, ok, err = c2.WaitForScriptValidated(context.Background(), "ns-1", "sc-2", time.Millisecond, time.Second)
	if err != nil || ok || sc.Status != "INVALID" {
		t.Fatalf("invalid script = %+v ok=%v err=%v", sc, ok, err)
	}
}

func TestWaitForRunTerminal(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		status := "RUNNING"
		if n > 2 {
			status = "COMPLETED"
		}
		_, _ = w.Write([]byte(`{"id":"lt-1","status":"` + status + `"}`))
	})
	lt, err := c.WaitForRun(context.Background(), "ns-1", "lt-1", time.Millisecond, time.Second)
	if err != nil || lt.Status != "COMPLETED" {
		t.Fatalf("run = %+v err=%v", lt, err)
	}

	// Every terminal status stops the loop (STOPPED/TIMED_OUT are not failures
	// of the wait itself — the CLI decides the exit code).
	for _, terminal := range []string{"FAILED", "STOPPED", "TIMED_OUT", "CLEANUP_FAILED"} {
		c2, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"id":"lt-2","status":"` + terminal + `"}`))
		})
		lt, err := c2.WaitForRun(context.Background(), "ns-1", "lt-2", time.Millisecond, time.Second)
		if err != nil || lt.Status != terminal {
			t.Fatalf("terminal %s: run=%+v err=%v", terminal, lt, err)
		}
	}
}

func TestUploadScript(t *testing.T) {
	var body map[string]string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/loadtest/v1/namespaces/ns-1/scripts" {
			t.Errorf("path = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"sc-1","name":"k6.js","status":"PENDING_VALIDATION"}`))
	})
	sc, err := c.UploadScript(context.Background(), "ns-1", "k6.js", "export default function(){}")
	if err != nil {
		t.Fatalf("UploadScript: %v", err)
	}
	if sc.ID != "sc-1" || body["name"] != "k6.js" || body["content"] == "" {
		t.Errorf("script=%+v body=%v", sc, body)
	}
}

func TestCreateLoadTestSendsLoadConfig(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"lt-1","status":"PROVISIONING","load_config":{"vusers":5}}`))
	})
	lt, err := c.CreateLoadTest(context.Background(), "ns-1", ltpclient.CreateLoadTestRequest{
		Name: "demo", ScriptID: "sc-1", LoadConfig: map[string]any{"vusers": 5, "duration": "30s"},
	})
	if err != nil {
		t.Fatalf("CreateLoadTest: %v", err)
	}
	cfg, ok := body["load_config"].(map[string]any)
	if !ok || cfg["vusers"] != float64(5) || cfg["duration"] != "30s" {
		t.Errorf("load_config = %v", body["load_config"])
	}
	if lt.Status != "PROVISIONING" {
		t.Errorf("run = %+v", lt)
	}
}

func TestDeleteAndStopAreMutations(t *testing.T) {
	var keys []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"x","status":"DELETING"}`))
	})
	if err := c.DeleteNamespace(context.Background(), "ns-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.StopLoadTest(context.Background(), "ns-1", "lt-1"); err != nil {
		t.Fatal(err)
	}
	for i, k := range keys {
		if k == "" {
			t.Errorf("call %d missing Idempotency-Key", i)
		}
	}
}

// TestDevAuthHeaders — the local/compose stack authenticates on the headers
// nginx normally injects after OIDC; the CLI must be able to set them.
func TestDevAuthHeaders(t *testing.T) {
	var tenant, user, role, auth string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		tenant, user, role = r.Header.Get("X-Tenant-Id"), r.Header.Get("X-User-Id"), r.Header.Get("X-User-Role")
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"namespaces":[]}`))
	})
	c.Token = "" // dev auth replaces the bearer token
	c.TenantID, c.UserID, c.Role = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "admin"
	if _, err := c.ListNamespaces(context.Background()); err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if tenant == "" || user == "" || role != "admin" {
		t.Errorf("identity headers = tenant %q user %q role %q", tenant, user, role)
	}
	if auth != "" {
		t.Errorf("dev auth must not also send a bearer token: %q", auth)
	}
}

// TestCreateWithSpecNestedShape — POST /namespaces with a spec returns
// {namespace, services, databases, ingress}; the name-only create returns the
// namespace flat. Both must land in the same CLI output.
func TestCreateWithSpecNestedShape(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"namespace":{"id":"ns-7","name":"spec","status":"DRAFT"},
			"services":["svc-1"],"databases":["db-1"],"ingress":[]}`))
	})
	res, err := c.CreateNamespaceWithSpec(context.Background(), ltpclient.CreateNamespaceRequest{
		Name: "spec", Spec: []byte(`{"services":[]}`),
	})
	if err != nil {
		t.Fatalf("CreateNamespaceWithSpec: %v", err)
	}
	if res.Namespace.ID != "ns-7" || res.Namespace.Status != "DRAFT" {
		t.Errorf("namespace = %+v", res.Namespace)
	}
	if len(res.Services) != 1 || len(res.Databases) != 1 {
		t.Errorf("components = %+v", res)
	}

	c2, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ns-8","name":"plain","status":"DRAFT"}`))
	})
	ns, err := c2.CreateNamespace(context.Background(), ltpclient.CreateNamespaceRequest{Name: "plain"})
	if err != nil || ns.ID != "ns-8" {
		t.Fatalf("flat create: ns=%+v err=%v", ns, err)
	}
}

// TestCreateNamespaceWithoutID — an empty body must not become a zero-value
// namespace that later commands use as an id.
func TestCreateNamespaceWithoutID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"count":0}`))
	})
	if _, err := c.CreateNamespace(context.Background(), ltpclient.CreateNamespaceRequest{Name: "x"}); err == nil {
		t.Fatal("expected an error for a response without a namespace id")
	}
}
