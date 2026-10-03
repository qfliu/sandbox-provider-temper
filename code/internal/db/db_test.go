package db

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/dbtest"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool := dbtest.RawPool(t)
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
	pool := dbtest.RawPool(t)
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

// The platform's role needs no superuser rights (D47): a plain role that
// owns its schema can run every migration, and can't do what only
// superusers can (shell commands from SQL, creating roles).
func TestMigrateAsPlainOwner(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	// Closed by the cleanup below, after it drops the role and schema: a
	// deferred Close ran first and the drops silently failed, leaking a
	// role and a schema per run (Phase 5 batch review).
	suffix := fmt.Sprint(time.Now().UnixNano())
	role, schema := "test_app_"+suffix, "test_app_"+suffix
	for _, q := range []string{
		"CREATE ROLE " + role + " NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE",
		"CREATE SCHEMA " + schema + " AUTHORIZATION " + role,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		defer admin.Close()
		for _, q := range []string{"DROP SCHEMA " + schema + " CASCADE", "DROP OWNED BY " + role, "DROP ROLE " + role} {
			if _, err := admin.Exec(context.Background(), q); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["role"] = role // act as the plain role
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if super, err := IsSuperuser(ctx, pool); err != nil || super {
		t.Fatalf("IsSuperuser = %v, %v; want false for the plain role", super, err)
	}
	if super, err := IsSuperuser(ctx, admin); err != nil || !super {
		t.Fatalf("IsSuperuser(admin) = %v, %v; the test database URL should be a superuser", super, err)
	}
	if applied, err := Migrate(ctx, pool); err != nil || len(applied) == 0 {
		t.Fatalf("migrations as a plain owner: %v, %v", applied, err)
	}
	for _, q := range []string{
		"COPY (SELECT 1) TO PROGRAM 'true'",
		"CREATE ROLE evil_" + suffix + " SUPERUSER",
		"ALTER ROLE " + role + " SUPERUSER",
	} {
		if _, err := pool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "must be superuser") {
			t.Errorf("as the platform role, %q: err = %v; want refused", q, err)
		}
	}
}
