package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool connects to TEST_DATABASE_URL inside a fresh, throwaway schema so
// tests never touch real data. Skips when TEST_DATABASE_URL isn't set; see
// code/scripts/test-db.sh.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())

	admin, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	first, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("first run applied no migrations")
	}
	second, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("second run re-applied %v", second)
	}
}

func TestSchemaConstraints(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var userID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (name) VALUES ('alice') RETURNING id").Scan(&userID); err != nil {
		t.Fatal(err)
	}

	insert := func(id, image, status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sandboxes
			(id, user_id, image, status, ssh_public_key, ttl_seconds, expires_at)
			VALUES ($1, $2, $3, $4, 'ssh-ed25519 AAAA', 3600, now() + interval '1 hour')`,
			id, userID, image, status)
		return err
	}

	if err := insert("sb-ok", "ubuntu-dev", "pending"); err != nil {
		t.Fatalf("valid sandbox rejected: %v", err)
	}
	if err := insert("sb-bad-image", "alpine", "pending"); err == nil {
		t.Fatal("image outside the allowed two was accepted")
	}
	if err := insert("sb-bad-status", "ubuntu-base", "exploded"); err == nil {
		t.Fatal("unknown status was accepted")
	}
	if _, err := pool.Exec(ctx, "INSERT INTO users (name) VALUES ('alice')"); err == nil {
		t.Fatal("duplicate user name was accepted")
	}
}
