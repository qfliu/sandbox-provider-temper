package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
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
