// Package store provides the PostgreSQL data layer: GORM connection for
// queries and a migration runner. sql/ migrations are the single source of
// truth for the schema.
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
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ErrNoMigrations is returned by Migrate when no migration files exist.
var ErrNoMigrations = errors.New("store: no migration files found")

var migrationNameRe = regexp.MustCompile(`^[0-9]+_[a-z0-9_]+\.sql$`)

// New opens a GORM connection with sane production settings.
func New(ctx context.Context, dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:  logger.Default.LogMode(logger.Warn),
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("store: sql db handle: %w", err)
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetConnMaxLifetime(time.Hour)
	sqlDB.SetConnMaxIdleTime(15 * time.Minute)
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return db.WithContext(ctx), nil
}

// Ping checks database reachability (used by /ready, fail-closed).
func Ping(ctx context.Context, db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return sqlDB.PingContext(ctx)
}

// Migrate applies all embedded migrations in filename order exactly once,
// tracked in tbl_schema_migrations. Migration files manage their own
// transactions (BEGIN/COMMIT inside the file), so each file runs on a bare
// connection with simple protocol (multi-statement support). GORM is not
// used here deliberately.
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
	body, err := files.ReadFile("sql/" + name)
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
