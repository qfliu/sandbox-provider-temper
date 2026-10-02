package db

import (
	"context"
	"testing"

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
