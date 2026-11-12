//go:build integration

package integration

import (
	"net/http"
	"testing"
	"time"
)

// INT-GW-01 — health qua toàn chuỗi nginx → unix socket → controller.
func TestGatewayHealth(t *testing.T) {
	requireStack(t)

	resp, err := http.Get(baseURL() + "/health")
	if err != nil {
		t.Fatalf("GET %s/health: %v", baseURL(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health: status %d, want 200", resp.StatusCode)
	}
}

// INT-GW-02 — Dex OIDC discovery reachable từ host (compose port-forward 5556).
func TestDexDiscovery(t *testing.T) {
	requireStack(t)

	ctx, cancel := ctxWithTimeout(10 * time.Second)
	defer cancel()
	_, err := dexDiscovery(ctx)
	if err != nil {
		t.Fatalf("dex discovery: %v", err)
	}
}
