//go:build integration

// Tenant fixture seed — pattern tests/e2e-smoke.sh nhưng qua pgx trực tiếp.
// Tenant "acme" (khớp secrets LTP_TENANT_ACME_* trong compose env) + host
// "localhost" + auth profile "default" trỏ Dex in-network.
package integration

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // driver "pgx"
)

const (
	seedTenantSlug = "acme"
	seedHost       = "localhost" // nginx $host đã strip port
	seedProfile    = "default"
	// issuer/jwks in-network (nginx container resolve "dex" qua compose DNS)
	seedIssuer = "http://dex:5556/dex"
	seedJWKS   = "http://dex:5556/dex/keys"
	seedClient = dexClientID
)

func seedTenantFixture(ctx context.Context) error {
	pool, err := pgxpool.New(ctx, pgDSN())
	if err != nil {
		return fmt.Errorf("pg connect: %w", err)
	}
	defer pool.Close()

	const q = `
WITH t AS (
  INSERT INTO tbl_tenants (name, slug) VALUES ('Acme', 'acme')
  ON CONFLICT (slug) DO UPDATE SET name='Acme' RETURNING id
), h AS (
  INSERT INTO tbl_tenant_hosts (tenant_id, host)
  SELECT id, '` + seedHost + `' FROM t
  ON CONFLICT (host) DO UPDATE SET tenant_id = EXCLUDED.tenant_id
), p AS (
  INSERT INTO tbl_tenant_auth_profiles (tenant_id, name, issuer, client_id, jwks_uri, config)
  SELECT id, '` + seedProfile + `', '` + seedIssuer + `', '` + seedClient + `', '` + seedJWKS + `',
         '{"jwt_mode":"jwks","oidc_expected_alg":"RS256"}'::jsonb
  FROM t
  ON CONFLICT (tenant_id, name) DO UPDATE
    SET issuer = EXCLUDED.issuer, client_id = EXCLUDED.client_id,
        jwks_uri = EXCLUDED.jwks_uri, config = EXCLUDED.config, updated_at = now()
)
SELECT 1`
	if _, err := pool.Exec(ctx, q); err != nil {
		return fmt.Errorf("seed tenant fixture: %w", err)
	}
	return nil
}
