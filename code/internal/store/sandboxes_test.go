package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newSandbox(userID string, n int) NewSandbox {
	return NewSandbox{
		ID:           fmt.Sprintf("sb-test%04d", n),
		UserID:       userID,
		Image:        "ubuntu-base",
		SSHPublicKey: "ssh-ed25519 AAAA",
		TTLSeconds:   3600,
	}
}

func TestCreateSandboxEnforcesQuota(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	s.pool.Exec(ctx, "UPDATE users SET max_sandboxes = 2 WHERE id = $1", alice.ID)

	for i := 0; i < 2; i++ {
		if _, err := s.CreateSandbox(ctx, newSandbox(alice.ID, i)); err != nil {
			t.Fatalf("sandbox %d: %v", i, err)
		}
	}
	if _, err := s.CreateSandbox(ctx, newSandbox(alice.ID, 2)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("third sandbox: err = %v, want ErrQuotaExceeded", err)
	}

	// Ending a sandbox frees its slot.
	if _, err := s.MarkSandboxEnded(ctx, "sb-test0000", StatusDeleted, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSandbox(ctx, newSandbox(alice.ID, 3)); err != nil {
		t.Fatalf("after freeing a slot: %v", err)
	}
}

// Many simultaneous creates must never exceed the quota (the FOR UPDATE lock).
func TestCreateSandboxQuotaUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false) // max_sandboxes = 10

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, quota := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateSandbox(ctx, newSandbox(alice.ID, i))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrQuotaExceeded):
				quota++
			default:
				t.Errorf("create %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 10 || quota != 10 {
		t.Fatalf("succeeded=%d quota_exceeded=%d, want 10 and 10", ok, quota)
	}
}

func TestSandboxRowDefaults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)

	sb, err := s.CreateSandbox(ctx, newSandbox(alice.ID, 0))
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusPending || sb.PodName != sb.ID {
		t.Fatalf("status=%q pod_name=%q, want pending and pod_name = id", sb.Status, sb.PodName)
	}
	if got := sb.ExpiresAt.Sub(sb.CreatedAt).Seconds(); got < 3599 || got > 3601 {
		t.Fatalf("expires_at - created_at = %vs, want 3600", got)
	}
}

func TestListAndMarkEnded(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	bob, _ := s.CreateUser(ctx, "bob", false)
	s.CreateSandbox(ctx, newSandbox(alice.ID, 1))
	s.CreateSandbox(ctx, newSandbox(alice.ID, 2))
	s.CreateSandbox(ctx, newSandbox(bob.ID, 3))

	first, err := s.MarkSandboxEnded(ctx, "sb-test0001", StatusDeleted, "first reason")
	if err != nil || first.Status != StatusDeleted || first.DeletedAt == nil {
		t.Fatalf("mark ended = %+v, %v", first, err)
	}
	again, _ := s.MarkSandboxEnded(ctx, "sb-test0001", StatusLost, "second reason")
	if again.Status != StatusDeleted || again.StatusReason != "first reason" {
		t.Fatalf("re-marking changed an ended sandbox: %+v", again)
	}

	live, _ := s.ListSandboxes(ctx, alice.ID, false)
	all, _ := s.ListSandboxes(ctx, alice.ID, true)
	if len(live) != 1 || len(all) != 2 {
		t.Fatalf("alice: live=%d all=%d, want 1 and 2 (bob's must not appear)", len(live), len(all))
	}
}

// mustExec runs setup SQL and fails the test if it errors, so a broken
// setup can't make a test pass for the wrong reason.
func mustExec(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("setup %q: %v", sql, err)
	}
}

func TestClaimForReaping(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	mk := func(n int) string {
		sb, err := s.CreateSandbox(ctx, newSandbox(alice.ID, n))
		if err != nil {
			t.Fatal(err)
		}
		return sb.ID
	}
	expired, idle, both, active, deleting, ended := mk(1), mk(2), mk(3), mk(4), mk(5), mk(6)
	mustExec(t, s, "UPDATE sandboxes SET expires_at = now() - interval '1 second' WHERE id = ANY($1)",
		[]string{expired, both})
	mustExec(t, s, "UPDATE sandboxes SET last_activity_at = now() - interval '31 minutes' WHERE id = ANY($1)",
		[]string{idle, both, deleting, ended})
	if err := s.MarkSandboxDeleting(ctx, deleting); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSandboxEnded(ctx, ended, StatusDeleted, "earlier"); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimForReaping(ctx, 30*time.Minute, "idle: no activity for 30m0s")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, sb := range claimed {
		got[sb.ID] = sb.StatusReason
		if sb.Status != StatusDeleting {
			t.Errorf("%s claimed with status %q, want deleting", sb.ID, sb.Status)
		}
	}
	want := map[string]string{
		expired: "expired: TTL reached",
		idle:    "idle: no activity for 30m0s",
		both:    "expired: TTL reached", // expiry wins when both apply
	}
	if len(got) != len(want) {
		t.Fatalf("claimed %v; want %v", got, want)
	}
	for id, reason := range want {
		if got[id] != reason {
			t.Errorf("%s: reason %q, want %q", id, got[id], reason)
		}
	}
	if _, ok := got[active]; ok {
		t.Fatal("an active sandbox was claimed")
	}
	again, err := s.ClaimForReaping(ctx, 30*time.Minute, "idle")
	if err != nil || len(again) != 0 {
		t.Fatalf("second claim re-claimed %d sandboxes (%v)", len(again), err)
	}
}

// Replicas reaping at the same moment must never both claim a sandbox. To
// make the race real (not just likely), another transaction first locks
// every row; the claimers all start, block on those locks, and are released
// together onto the same rows.
func TestClaimForReapingIsExclusive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	mustExec(t, s, "UPDATE users SET max_sandboxes = 50 WHERE id = $1", alice.ID)
	for i := 0; i < 40; i++ {
		if _, err := s.CreateSandbox(ctx, newSandbox(alice.ID, i)); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, s, "UPDATE sandboxes SET expires_at = now() - interval '1 second'")

	gate, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, "SELECT 1 FROM sandboxes FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var returned atomic.Int32
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := s.ClaimForReaping(ctx, time.Hour, "idle")
			returned.Add(1)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, sb := range claimed {
				seen[sb.ID]++
			}
		}()
	}
	time.Sleep(300 * time.Millisecond) // let all four reach the lock
	if n := returned.Load(); n != 0 {
		t.Fatalf("%d claimers finished while the rows were locked; they weren't racing", n)
	}
	if err := gate.Commit(ctx); err != nil { // release all four at once
		t.Fatal(err)
	}
	wg.Wait()

	if len(seen) != 40 {
		t.Fatalf("claimed %d distinct sandboxes, want 40", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s claimed %d times", id, n)
		}
	}
}

// API activity: TouchSandbox only moves running sandboxes, at most every
// 10 s.
func TestTouchSandbox(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	running, _ := s.CreateSandbox(ctx, newSandbox(alice.ID, 1))
	pending, _ := s.CreateSandbox(ctx, newSandbox(alice.ID, 2))
	mustExec(t, s, "UPDATE sandboxes SET status = 'running' WHERE id = $1", running.ID)
	mustExec(t, s, "UPDATE sandboxes SET last_activity_at = now() - interval '1 hour'")

	if err := s.TouchSandbox(ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSandbox(ctx, pending.ID); err != nil {
		t.Fatal(err)
	}
	if got := idleSecs(t, s, running.ID); got > 5 {
		t.Fatalf("running sandbox not touched (idle %.0fs)", got)
	}
	if got := idleSecs(t, s, pending.ID); got < 3500 {
		t.Fatalf("non-running sandbox was touched (idle %.0fs)", got)
	}

	// Throttle: a second touch right away doesn't move it again.
	mustExec(t, s, "UPDATE sandboxes SET last_activity_at = now() - interval '5 seconds' WHERE id = $1", running.ID)
	s.TouchSandbox(ctx, running.ID)
	if got := idleSecs(t, s, running.ID); got < 4 {
		t.Fatalf("touched again within 10s (idle %.0fs)", got)
	}
}

// idleSecs is how long ago the sandbox was last used, as every reader
// (API responses, the reaper) sees it: API or SSH activity, whichever is
// newer.
func idleSecs(t *testing.T, s *Store, id string) float64 {
	t.Helper()
	sb, err := s.GetSandbox(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return time.Since(sb.LastActivityAt).Seconds()
}

// SSH activity (D36) counts for readers and the reaper, is throttled, and
// a bogus value written with stolen gateway credentials can neither break
// reads nor get a sandbox reaped.
func TestSSHActivity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	sb, _ := s.CreateSandbox(ctx, newSandbox(alice.ID, 1))
	mustExec(t, s, "UPDATE sandboxes SET status = 'running', last_activity_at = now() - interval '1 hour' WHERE id = $1", sb.ID)
	claimed := func() bool {
		t.Helper()
		list, err := s.ClaimForReaping(ctx, 30*time.Minute, "idle")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range list {
			if c.ID == sb.ID {
				mustExec(t, s, "UPDATE sandboxes SET status = 'running', status_reason = '' WHERE id = $1", sb.ID)
				return true
			}
		}
		return false
	}

	if err := s.RecordSSHActivity(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if got := idleSecs(t, s, sb.ID); got > 5 {
		t.Fatalf("SSH activity not seen by readers (idle %.0fs)", got)
	}
	if claimed() {
		t.Fatal("sandbox in SSH use was reaped")
	}

	// Throttle: a second write within 10 s doesn't move it.
	mustExec(t, s, "UPDATE ssh_activity SET last_seen_at = now() - interval '5 seconds'")
	s.RecordSSHActivity(ctx, sb.ID)
	if got := idleSecs(t, s, sb.ID); got < 4 {
		t.Fatalf("recorded again within 10s (idle %.0fs)", got)
	}

	// Far-future value: reads still work and see "now", not the future.
	mustExec(t, s, "UPDATE ssh_activity SET last_seen_at = 'infinity'")
	if got := idleSecs(t, s, sb.ID); got < -5 || got > 5 {
		t.Fatalf("infinity not capped at now (idle %.0fs)", got)
	}
	// Ancient value: can't make a recently used sandbox look idle…
	mustExec(t, s, "UPDATE ssh_activity SET last_seen_at = '-infinity'")
	mustExec(t, s, "UPDATE sandboxes SET last_activity_at = now() WHERE id = $1", sb.ID)
	if claimed() {
		t.Fatal("an ancient SSH time got a recently used sandbox reaped")
	}
	// …and a sandbox idle on both counts is reaped as usual.
	mustExec(t, s, "UPDATE sandboxes SET last_activity_at = now() - interval '1 hour' WHERE id = $1", sb.ID)
	if !claimed() {
		t.Fatal("idle sandbox not reaped")
	}

	if err := s.RecordSSHActivity(ctx, "sb-000000000000"); err == nil {
		t.Fatal("recorded SSH activity for a sandbox that doesn't exist")
	}
}

// The gateway's database role (deploy-gateway.sh grants) must not be able to
// change the sandboxes table, and doesn't need to: recording SSH activity
// works with its own grants alone. Runs as a throwaway role with exactly
// those grants.
func TestGatewayRoleCannotChangeSandboxes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	sb, _ := s.CreateSandbox(ctx, newSandbox(alice.ID, 1))

	role := fmt.Sprintf("test_gateway_%d", time.Now().UnixNano())
	var schema string
	if err := s.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, "CREATE ROLE "+role+" NOLOGIN")
	t.Cleanup(func() {
		s.pool.Exec(context.Background(), "DROP OWNED BY "+role)
		s.pool.Exec(context.Background(), "DROP ROLE "+role)
	})
	mustExec(t, s, "GRANT USAGE ON SCHEMA "+schema+" TO "+role)
	mustExec(t, s, "GRANT SELECT ON sandboxes TO "+role)
	mustExec(t, s, "GRANT SELECT, INSERT, UPDATE ON ssh_activity TO "+role)

	// asGateway runs q as the role, in a transaction that is rolled back.
	asGateway := func(q string, args ...any) error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, q, args...)
		return err
	}
	for _, q := range []string{
		"UPDATE sandboxes SET last_activity_at = '-infinity'",
		"UPDATE sandboxes SET status = 'deleting'",
		"DELETE FROM sandboxes",
		"SELECT * FROM users",
		"SELECT * FROM api_keys",
	} {
		if err := asGateway(q); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("as the gateway role, %q: err = %v; want permission denied", q, err)
		}
	}
	// The gateway reads sandbox rows with every column (GetSandbox), incl.
	// the created-by key copied onto the row (step 4.5).
	if err := asGateway("SELECT " + sandboxColumns + " FROM sandboxes"); err != nil {
		t.Fatalf("the gateway role can't read sandbox rows: %v", err)
	}
	if err := asGateway(`INSERT INTO ssh_activity (sandbox_id, last_seen_at) VALUES ($1, now())
		ON CONFLICT (sandbox_id) DO UPDATE SET last_seen_at = now()
		WHERE ssh_activity.last_seen_at < now() - interval '10 seconds'`, sb.ID); err != nil {
		t.Fatalf("the gateway role can't record SSH activity: %v", err)
	}
}

// The key that created a sandbox is stored on it, survives the key being
// revoked, and shows in every reader; sandboxes without one have none.
func TestSandboxCreatedByKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	plain, k, _ := s.CreateAPIKey(ctx, alice.ID, "ci-runner")
	s.CreateAPIKey(ctx, alice.ID, "other") // so k can be revoked

	u, ref, err := s.Authenticate(ctx, plain)
	if err != nil || u.ID != alice.ID || ref.ID != k.ID || ref.Name != "ci-runner" || ref.Prefix != k.Prefix {
		t.Fatalf("Authenticate = %+v, %+v, %v", u, ref, err)
	}
	n := newSandbox(alice.ID, 1)
	n.CreatedByKey = &ref
	sb, err := s.CreateSandbox(ctx, n)
	if err != nil || sb.CreatedByKey == nil || *sb.CreatedByKey != ref {
		t.Fatalf("created = %+v, %v; want CreatedByKey %+v", sb.CreatedByKey, err, ref)
	}
	if err := s.RevokeAPIKey(ctx, alice.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSandbox(ctx, sb.ID)
	if got.CreatedByKey == nil || got.CreatedByKey.Name != "ci-runner" {
		t.Fatalf("after revoking the key: %+v", got.CreatedByKey)
	}
	all, _ := s.ListAllSandboxes(ctx, true)
	if len(all) != 1 || all[0].CreatedByKey == nil || all[0].CreatedByKey.ID != k.ID {
		t.Fatalf("admin list: %+v", all)
	}
	old, _ := s.CreateSandbox(ctx, newSandbox(alice.ID, 2)) // no key given
	if old.CreatedByKey != nil {
		t.Fatalf("sandbox without a key has %+v", old.CreatedByKey)
	}
}
