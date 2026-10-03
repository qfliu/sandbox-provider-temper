package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

func TestListAndRevokeAPIKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	bob, _ := s.CreateUser(ctx, "bob", false)
	plain1, k1, _ := s.CreateAPIKey(ctx, alice.ID, "laptop")
	_, k2, _ := s.CreateAPIKey(ctx, alice.ID, "agent")
	_, bobKey, _ := s.CreateAPIKey(ctx, bob.ID, "bob")

	keys, err := s.ListAPIKeys(ctx, alice.ID)
	if err != nil || len(keys) != 2 || keys[0].ID != k2.ID {
		t.Fatalf("keys = %+v, %v; want alice's 2 keys, newest first", keys, err)
	}

	// Someone else's key, a malformed id, and an unknown id all look the same.
	for _, id := range []string{bobKey.ID, "not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if err := s.RevokeAPIKey(ctx, alice.ID, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("revoke %q as alice: %v, want ErrNotFound", id, err)
		}
	}
	if err := s.RevokeAPIKey(ctx, alice.ID, k1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByAPIKey(ctx, plain1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked key still authenticates: %v", err)
	}
	if err := s.RevokeAPIKey(ctx, alice.ID, k1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking twice: %v, want ErrNotFound", err)
	}
	// k2 is now alice's only key: revoking it would lock her out.
	if err := s.RevokeAPIKey(ctx, alice.ID, k2.ID); !errors.Is(err, ErrLastKey) {
		t.Fatalf("revoking the last key: %v, want ErrLastKey", err)
	}
	if keys, _ := s.ListAPIKeys(ctx, alice.ID); len(keys) != 1 || keys[0].ID != k2.ID {
		t.Fatalf("after revoke, keys = %+v", keys)
	}
}

// Two concurrent revokes of a user's last two keys must not both succeed.
// Many users at once, so the race window is actually hit without the lock.
func TestRevokeLastTwoKeysConcurrently(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const users = 40
	type pair struct{ user, k1, k2 string }
	pairs := make([]pair, users)
	for i := range pairs {
		u, err := s.CreateUser(ctx, fmt.Sprintf("u%d", i), false)
		if err != nil {
			t.Fatal(err)
		}
		_, k1, _ := s.CreateAPIKey(ctx, u.ID, "a")
		_, k2, _ := s.CreateAPIKey(ctx, u.ID, "b")
		pairs[i] = pair{u.ID, k1.ID, k2.ID}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, p := range pairs {
		for _, id := range []string{p.k1, p.k2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				s.RevokeAPIKey(ctx, p.user, id)
			}()
		}
	}
	close(start)
	wg.Wait()
	for i, p := range pairs {
		if keys, _ := s.ListAPIKeys(ctx, p.user); len(keys) != 1 {
			t.Errorf("user %d has %d active keys, want 1", i, len(keys))
		}
	}
}

func TestListUsersWithCounts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	root, _ := s.CreateUser(ctx, "root", true)
	s.CreateAPIKey(ctx, alice.ID, "a")
	_, k2, _ := s.CreateAPIKey(ctx, alice.ID, "b")
	s.CreateAPIKey(ctx, alice.ID, "c")
	if err := s.RevokeAPIKey(ctx, alice.ID, k2.ID); err != nil {
		t.Fatal(err)
	}
	live, _ := s.CreateSandbox(ctx, newSandbox(alice.ID, 1))
	ended, _ := s.CreateSandbox(ctx, newSandbox(alice.ID, 2))
	if _, err := s.MarkSandboxEnded(ctx, ended.ID, StatusDeleted, "test"); err != nil {
		t.Fatal(err)
	}

	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("users = %+v, %v", users, err)
	}
	a, r := users[0], users[1]
	if a.Name != "alice" || a.LiveSandboxes != 1 || a.ActiveKeys != 2 || a.MaxSandboxes != 10 {
		t.Errorf("alice = %+v; want 1 live sandbox, 2 active keys", a)
	}
	if r.Name != "root" || !r.IsAdmin || r.LiveSandboxes != 0 || r.ActiveKeys != 0 || r.ID != root.ID {
		t.Errorf("root = %+v", r)
	}

	all, err := s.ListAllSandboxes(ctx, false)
	if err != nil || len(all) != 1 || all[0].ID != live.ID || all[0].Owner != "alice" {
		t.Fatalf("live sandboxes = %+v, %v; want alice's live one with its owner", all, err)
	}
	if all, _ := s.ListAllSandboxes(ctx, true); len(all) != 2 {
		t.Fatalf("with ended: %d sandboxes, want 2", len(all))
	}
}
