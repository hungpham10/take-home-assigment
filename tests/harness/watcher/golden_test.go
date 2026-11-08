//go:build harness_k8s

package watcher_harness

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/mockcontrol"
	"github.com/hungpham/loadtest-platform/pkgs/statuswatcher"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

// TestWATCH07GoldenPath — Ưu tiên 3 của audit test (harness-testing.md §14.2):
// test xuyên suốt một line: HTTP API POST /apply → Env Reconciler apply thật
// lên cluster → Status Watcher projection → GET /readiness ready:true.
// Trước đây hai phần được test tách biệt (API tests không có cluster;
// WATCH-* tests không có HTTP API).
func TestWATCH07GoldenPath(t *testing.T) {
	if sharedDB == nil || sharedNSID == "" {
		t.Skip("harness not set up")
	}
	client, err := harnessClient()
	if err != nil {
		t.Skipf("no cluster: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Composition root thật: HTTP server (K8S mode) + hai control loop.
	h := testutil.NewServer(t, testutil.ServerDeps{DB: sharedDB, Now: time.Now, K8S: true})

	rc := envreconciler.New(client, envreconciler.Options{})
	mockcontrol.Attach(rc)
	go rc.Start(ctx, envreconciler.LoadEnvs(sharedDB))

	cfg := statuswatcher.DefaultConfig()
	cfg.Resync = 5 * time.Second
	w := statuswatcher.New(client, sharedDB, cfg)
	go w.Start(ctx)

	principal := testutil.TenantUserHeaders(t, sharedDB, "user")
	call := func(method, path string, headers map[string]string, body any) map[string]any {
		rec := testutil.Do(t, h, method, path, headers, body)
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp
	}

	// 1. Create namespace + service qua API thật.
	nsName := fmt.Sprintf("golden-%d", time.Now().UnixNano())
	ns := call("POST", "/api/loadtest/v1/namespaces", testutil.WithKey(principal, "golden-ns"), map[string]string{"name": nsName})
	nsID, _ := ns["id"].(string)
	if nsID == "" {
		t.Fatalf("create ns failed: %v", ns)
	}

	svc := call("POST", "/api/loadtest/v1/namespaces/"+nsID+"/services", principal, map[string]any{
		"name": "golden-svc", "spec": map[string]any{"openapi": map[string]any{"content": goldenSpec}},
	})
	if svc["id"] == nil && svc["service_id"] == nil {
		t.Fatalf("create service failed: %v", svc)
	}

	// 2. validate → APPLYING; apply → 202 (async hand-off, INT-NS-25).
	if v := call("POST", "/api/loadtest/v1/namespaces/"+nsID+"/validate", principal, nil); v["status"] == nil {
		t.Fatalf("validate failed: %v", v)
	}
	applied := call("POST", "/api/loadtest/v1/namespaces/"+nsID+"/apply", testutil.WithKey(principal, "golden-apply"), nil)
	if applied["status"] != "APPLYING" {
		t.Fatalf("apply: %v, want 202 APPLYING", applied)
	}

	// 3. Hai control loops đóng vòng: đợi GET /readiness ready:true.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		rec := testutil.Do(t, h, "GET", "/api/loadtest/v1/namespaces/"+nsID+"/readiness", principal, nil)
		var resp struct {
			Status           string         `json:"status"`
			Ready            bool           `json:"ready"`
			ReadyForLoadtest bool           `json:"ready_for_loadtest"`
			Components       map[string]any `json:"components"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &resp) == nil && resp.Ready && resp.ReadyForLoadtest && resp.Status == "READY" {
			// Projection phải có service READY (không phải readinessGates
			// on-the-fly).
			svcBlock, ok := resp.Components["services"].(map[string]any)
			if !ok {
				t.Fatal("readiness payload without projection blocks")
			}
			if svcBlock["ready"].(float64) != 1 {
				t.Fatalf("service block not projection-backed: %v", svcBlock)
			}
			return // golden path hoàn tất end-to-end
		}
		time.Sleep(2 * time.Second)
	}
	var status string
	_ = sharedDB.Raw(`SELECT status FROM tbl_namespaces WHERE id = ?`, nsID).Scan(&status)
	t.Fatalf("golden path did not converge (ns status %s)", status)
}

const goldenSpec = `{"openapi":"3.0.0","info":{"title":"Golden","version":"1"},"paths":{"/x":{"get":{"responses":{"200":{"description":"ok"}}}}}}`
