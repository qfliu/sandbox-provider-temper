package store

import (
	"context"
	"errors"
	"testing"

	"github.com/qfliu/sandbox-provider-temper/code/internal/db"
	"github.com/qfliu/sandbox-provider-temper/code/internal/dbtest"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	pool := dbtest.RawPool(t)
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return New(pool)
}

func TestCreateUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u, err := s.CreateUser(ctx, "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == "" || u.Name != "alice" || !u.IsAdmin || u.MaxSandboxes != 10 {
		t.Fatalf("unexpected user %+v", u)
	}
	if _, err := s.CreateUser(ctx, "alice", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: err = %v, want ErrConflict", err)
	}
	got, err := s.UserByName(ctx, "alice")
	if err != nil || got.ID != u.ID {
		t.Fatalf("UserByName = %+v, %v", got, err)
	}
	if _, err := s.UserByName(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: err = %v, want ErrNotFound", err)
	}
}

func TestAPIKeyRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)

	key, meta, err := s.CreateAPIKey(ctx, alice.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Prefix != key[:len(meta.Prefix)] || meta.UserID != alice.ID {
		t.Fatalf("unexpected key metadata %+v", meta)
	}

	got, err := s.UserByAPIKey(ctx, key)
	if err != nil || got.ID != alice.ID {
		t.Fatalf("UserByAPIKey = %+v, %v; want alice", got, err)
	}

	var lastUsed *string
	if err := s.pool.QueryRow(ctx, "SELECT last_used_at::text FROM api_keys WHERE id = $1", meta.ID).Scan(&lastUsed); err != nil || lastUsed == nil {
		t.Fatalf("last_used_at not recorded: %v, %v", lastUsed, err)
	}

	var stored []byte
	s.pool.QueryRow(ctx, "SELECT key_hash FROM api_keys WHERE id = $1", meta.ID).Scan(&stored)
	if string(stored) == key {
		t.Fatal("plain key stored in the database")
	}
}

func TestUserByAPIKeyRejects(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	key, meta, _ := s.CreateAPIKey(ctx, alice.ID, "")

	// Same as the real key except for its last character.
	last := byte('A')
	if key[len(key)-1] == 'A' {
		last = 'B'
	}
	almost := key[:len(key)-1] + string(last)

	for name, k := range map[string]string{
		"empty":            "",
		"garbage":          "not-a-key",
		"one char off":     almost,
		"well-formed fake": "sk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		if _, err := s.UserByAPIKey(ctx, k); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}

	if _, err := s.pool.Exec(ctx, "UPDATE api_keys SET revoked_at = now() WHERE id = $1", meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByAPIKey(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked key: err = %v, want ErrNotFound", err)
	}
}

// Recording last_used_at is bookkeeping: if that write fails, a valid key
// must still authenticate (review finding M8).
func TestUserByAPIKeySurvivesLastUsedWriteFailure(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	key, _, _ := s.CreateAPIKey(ctx, alice.ID, "")

	// Make every UPDATE of api_keys fail, while reads keep working.
	if _, err := s.pool.Exec(ctx, `
		CREATE FUNCTION refuse_update() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'simulated write failure'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER refuse BEFORE UPDATE ON api_keys FOR EACH ROW EXECUTE FUNCTION refuse_update();`); err != nil {
		t.Fatal(err)
	}

	got, err := s.UserByAPIKey(ctx, key)
	if err != nil || got.ID != alice.ID {
		t.Fatalf("valid key rejected because last_used_at couldn't be written: %+v, %v", got, err)
	}
}
