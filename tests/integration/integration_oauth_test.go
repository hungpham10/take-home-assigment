//go:build integration

package integration

import (
	"net/http"
	"testing"
	"time"
)

// INT-OAUTH-01 — mint id_token từ Dex (HTML login flow) rồi dùng làm bearer JWT
// qua nginx: gateway xác minh JWT theo tenant config (jwks_uri = Dex, RS256),
// inject X-Tenant-Id/X-User-Id rồi proxy tới controller ⇒ 200.
func TestOAuthBearerJWTThroughGateway(t *testing.T) {
	requireStack(t)

	ctx, cancel := ctxWithTimeout(60 * time.Second)
	defer cancel()

	if err := seedTenantFixture(ctx); err != nil {
		t.Fatalf("seed tenant fixture: %v", err)
	}

	idToken, err := dexLoginGetIDToken(ctx)
	if err != nil {
		t.Fatalf("dex login: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/api/loadtest/v1/namespaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = seedHost // nginx tenant resolution theo $host
	req.Header.Set("Authorization", "Bearer "+idToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET namespaces: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET namespaces with Dex id_token: status %d, want 200 (nginx JWT authn failed?)", resp.StatusCode)
	}
}

// INT-OAUTH-02 — bearer rác phải bị nginx chặn 401 (không lọt tới controller).
func TestOAuthInvalidBearerRejected(t *testing.T) {
	requireStack(t)

	req, err := http.NewRequest(http.MethodGet, baseURL()+"/api/loadtest/v1/namespaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = seedHost
	req.Header.Set("Authorization", "Bearer not-a-real-jwt")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET namespaces: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET namespaces with invalid bearer: status %d, want 401", resp.StatusCode)
	}
}
