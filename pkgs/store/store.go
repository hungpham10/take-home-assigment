// Package store provides the PostgreSQL connection pool and the migration
// runner. sql/ migrations are the single source of truth for the schema.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoMigrations is returned by Migrate when no migration files exist.
var ErrNoMigrations = errors.New("store: no migration files found")

var migrationNameRe = regexp.MustCompile(`^[0-9]+_[a-z0-9_]+\.sql$`)

// New opens a pgx connection pool with bounded lifetimes.
func New(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies all embedded migrations in filename order exactly once,
// tracked in tbl_schema_migrations. Migration files manage their own
// transactions (BEGIN/COMMIT inside the file), so each file runs on a bare
// connection with simple protocol (multi-statement support).
func Migrate(ctx context.Context, dsn string, files embed.FS) error {
	names, err := migrationNames(files, "sql")
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return ErrNoMigrations
	}
	mcfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("store: parse dsn: %w", err)
	}
	mcfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, mcfg)
	if err != nil {
		return fmt.Errorf("store: connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	for _, name := range names {
		applied, err := isApplied(ctx, conn, name)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := applyOne(ctx, conn, files, name); err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
	}
	return nil
}

func migrationNames(files embed.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return nil, fmt.Errorf("store: read embedded sql dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !migrationNameRe.MatchString(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// errUndefinedTable is the PostgreSQL SQLSTATE for a missing relation (42P01).
const errUndefinedTable = "42P01"

func tableMissing(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == errUndefinedTable
	}
	return false
}

func isApplied(ctx context.Context, conn *pgx.Conn, name string) (bool, error) {
	var n int
	err := conn.QueryRow(ctx,
		`SELECT count(*) FROM tbl_schema_migrations WHERE filename = $1`, name).Scan(&n)
	// tbl_schema_migrations lives inside 0001; before it exists treat as unapplied.
	if err != nil && !tableMissing(err) {
		return false, err
	}
	return n > 0, nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, files embed.FS, name string) error {
	body, err := files.ReadFile("sql/" + name) // dir is fixed "sql" for the real embed
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, string(body)); err != nil {
		return err
	}
	_, err = conn.Exec(ctx,
		`INSERT INTO tbl_schema_migrations (filename) VALUES ($1) ON CONFLICT (filename) DO NOTHING`,
		name)
	return err
}
