package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

func TestReapDeletesExpiredAndIdleSandboxes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	expired := f.newSandbox(t, false)
	idle := f.newSandbox(t, false)
	active := f.newSandbox(t, false)
	mustExec(t, f, "UPDATE sandboxes SET expires_at = now() - interval '1 second' WHERE id = $1", expired.ID)
	mustExec(t, f, "UPDATE sandboxes SET last_activity_at = now() - interval '31 minutes' WHERE id = $1", idle.ID)

	n, err := f.svc.Reap(ctx)
	if err != nil || n != 2 {
		t.Fatalf("reaped %d, %v; want 2", n, err)
	}
	for id, wantReason := range map[string]string{
		expired.ID: "expired: TTL reached",
		idle.ID:    "idle: no activity for 30m0s",
	} {
		got := f.status(t, id)
		if got.Status != store.StatusDeleted || got.StatusReason != wantReason {
			t.Errorf("%s: %q / %q, want deleted / %q", id, got.Status, got.StatusReason, wantReason)
		}
	}
	if got := f.status(t, active.ID).Status; got != store.StatusPending {
		t.Fatalf("active sandbox: status %q, want untouched", got)
	}
	deleted := map[string]bool{}
	for _, name := range f.pods.deleted {
		deleted[name] = true
	}
	if len(f.pods.deleted) != 2 || !deleted[expired.ID] || !deleted[idle.ID] {
		t.Fatalf("pods deleted = %v, want exactly %s and %s", f.pods.deleted, expired.ID, idle.ID)
	}
}

// If the pod delete fails, the sandbox stays "deleting" and the reconcile
// finishes it later, keeping the reaper's reason.
func TestReapFailureIsFinishedByReconcileWithReason(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb := f.newSandbox(t, true)
	mustExec(t, f, "UPDATE sandboxes SET expires_at = now() - interval '1 second' WHERE id = $1", sb.ID)

	f.pods.failDelete = true
	if _, err := f.svc.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.status(t, sb.ID); got.Status != store.StatusDeleting || got.StatusReason != "expired: TTL reached" {
		t.Fatalf("after failed pod delete: %q / %q", got.Status, got.StatusReason)
	}

	f.pods.failDelete = false
	y := f.syncer()
	y.SetCache(fakeCache{}) // the pod is gone by now
	y.Reconcile(ctx)
	if got := f.status(t, sb.ID); got.Status != store.StatusDeleted || got.StatusReason != "expired: TTL reached" {
		t.Fatalf("after reconcile: %q / %q, want deleted with the reaper's reason", got.Status, got.StatusReason)
	}
}

// A hung pod delete must not stall the rest of the reap.
func TestReapBoundsEachPodDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	stuck := f.newSandbox(t, true)
	ok := f.newSandbox(t, true)
	mustExec(t, f, "UPDATE sandboxes SET expires_at = now() - interval '1 second' WHERE id = ANY($1)",
		[]string{stuck.ID, ok.ID})

	defer func(old time.Duration) { reapDeleteTimeout = old }(reapDeleteTimeout)
	reapDeleteTimeout = 100 * time.Millisecond
	f.pods.hangDelete = map[string]bool{stuck.ID: true}

	start := time.Now()
	if _, err := f.svc.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("reap took %s; a hung delete stalled it", time.Since(start))
	}
	if got := f.status(t, ok.ID).Status; got != store.StatusDeleted {
		t.Fatalf("the sandbox after the hung one: status %q, want deleted", got)
	}
	if got := f.status(t, stuck.ID).Status; got != store.StatusDeleting {
		t.Fatalf("hung sandbox: status %q, want deleting (reconcile retries)", got)
	}
}

// mustExec runs setup SQL and fails the test if it errors, so a broken
// setup can't make a test pass for the wrong reason.
func mustExec(t *testing.T, f fixture, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("setup %q: %v", sql, err)
	}
}
