package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	expired, idle, active, deleting, ended := mk(1), mk(2), mk(3), mk(4), mk(5)
	s.pool.Exec(ctx, "UPDATE sandboxes SET expires_at = now() - interval '1 second' WHERE id = $1", expired)
	s.pool.Exec(ctx, "UPDATE sandboxes SET last_activity_at = now() - interval '31 minutes' WHERE id = ANY($1)",
		[]string{idle, deleting, ended})
	s.MarkSandboxDeleting(ctx, deleting)
	s.MarkSandboxEnded(ctx, ended, StatusDeleted, "earlier")

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
	if len(got) != 2 || got[expired] != "expired: TTL reached" || got[idle] != "idle: no activity for 30m0s" {
		t.Fatalf("claimed %v; want only the expired and idle sandboxes with their reasons", got)
	}
	if _, ok := got[active]; ok {
		t.Fatal("an active sandbox was claimed")
	}
	again, _ := s.ClaimForReaping(ctx, 30*time.Minute, "idle")
	if len(again) != 0 {
		t.Fatalf("second claim re-claimed %d sandboxes", len(again))
	}
}

// Two replicas reaping at the same moment must never both claim a sandbox.
func TestClaimForReapingIsExclusive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "alice", false)
	s.pool.Exec(ctx, "UPDATE users SET max_sandboxes = 50 WHERE id = $1", alice.ID)
	for i := 0; i < 40; i++ {
		s.CreateSandbox(ctx, newSandbox(alice.ID, i))
	}
	s.pool.Exec(ctx, "UPDATE sandboxes SET expires_at = now() - interval '1 second'")

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := s.ClaimForReaping(ctx, time.Hour, "idle")
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
