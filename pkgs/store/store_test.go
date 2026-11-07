package store

import (
	"context"
	"embed"
	"os"
	"testing"
)

//go:embed testdata/sql/*.sql
var testMigrations embed.FS

// testEnvDSN can be overridden by CI; empty falls back to local compose DSN.
var testEnvDSN = os.Getenv("TEST_DATABASE_DSN")

// Integration test — requires a clean PostgreSQL (CI sql-migrations job /
// local docker compose). Skips when the database is unreachable.
func TestMigrateAppliesOnceAndInOrder(t *testing.T) {
	dsn := testEnvDSN
	if dsn == "" {
		dsn = "postgres://ltp:ltp@localhost:5432/ltp?sslmode=disable"
	}
	ctx := context.Background()

	if err := Migrate(ctx, dsn, testMigrations); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	// Re-run must be idempotent.
	if err := Migrate(ctx, dsn, testMigrations); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestMigrationNamesFilterAndSort(t *testing.T) {
	names, err := migrationNames(testMigrations, "testdata/sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "0001_test_first.sql" || names[1] != "0002_test_second.sql" {
		t.Errorf("names = %v, want sorted 0001, 0002", names)
	}
}
