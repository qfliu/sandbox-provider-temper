// Package db connects to Postgres and applies schema migrations.
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration files are compiled into the binary, so the API never depends on
// files being present on disk.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockID is an arbitrary constant for pg_advisory_lock. With several
// API replicas starting at once, only one applies migrations; the others wait.
const migrationLockID = 727_100_001

// Open connects to Postgres and checks the connection works.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

// IsSuperuser reports whether the pool's role is a Postgres superuser. The
// API should connect as a plain owner role (sandbox_app, D47), so an SQL bug
// can't take over the database server.
func IsSuperuser(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var super bool
	err := pool.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super)
	return super, err
}

// Migrate applies every migration in migrations/ that hasn't run yet, in
// filename order, each in its own transaction. Applied versions are recorded
// in schema_migrations. Safe to call on every startup.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (applied []string, err error) {
	applied = []string{} // log as [] rather than null when nothing is new
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	// Advisory locks belong to a session, so lock and unlock on the same conn.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text        PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")

		var done bool
		if err := conn.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version,
		).Scan(&done); err != nil {
			return applied, err
		}
		if done {
			continue
		}

		sql, err := migrationFiles.ReadFile(name)
		if err != nil {
			return applied, err
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		}); err != nil {
			return applied, fmt.Errorf("migration %s: %w", version, err)
		}
		applied = append(applied, version)
	}
	return applied, nil
}
