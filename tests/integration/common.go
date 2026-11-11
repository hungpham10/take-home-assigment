//go:build integration

// Package integration — E2E suite chống lại compose stack thật (nginx gateway +
// Dex + valkey + postgres + controller), pattern sources/opsense-rs integration
// tests. Chạy bằng `make test-integration` (compose up --wait trước).
//
// Quy tắc opsense-rs: nếu stack không reachable thì SKIP, trừ khi
// LTP_INTEGRATION=1 (CI integration job) → fail hard để CI không green giả.
package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// BaseURL của gateway nginx (host-side).
func baseURL() string { return envOr("LTP_BASE_URL", "http://127.0.0.1:8080") }

// DexIssuer host-side (compose port-forward 5556).
func dexIssuer() string { return envOr("LTP_DEX_ISSUER", "http://localhost:5556/dex") }

// pgDSN host-side — dùng seed tenant/auth profile fixture.
func pgDSN() string {
	return envOr("LTP_PG_DSN", "postgres://ltp:ltp@127.0.0.1:5432/ltp?sslmode=disable")
}

// requireStack đảm bảo stack reachable: skip nếu không, FAIL HARD khi
// LTP_INTEGRATION=1 (chống green giả trong CI integration job).
func requireStack(t *testing.T) {
	t.Helper()
	if v := os.Getenv("LTP_INTEGRATION"); v == "1" {
		if err := waitHealth(30 * time.Second); err != nil {
			t.Fatalf("integration stack not reachable (LTP_INTEGRATION=1): %v", err)
		}
		return
	}
	if err := waitHealth(5 * time.Second); err != nil {
		t.Skipf("compose stack not reachable, skipping integration test: %v", err)
	}
}

func waitHealth(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL() + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	last, err := http.Get(baseURL() + "/health")
	if err != nil {
		return fmt.Errorf("GET %s/health: %w", baseURL(), err)
	}
	defer last.Body.Close()
	return fmt.Errorf("GET %s/health: status %d", baseURL(), last.StatusCode)
}

func ctxWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
