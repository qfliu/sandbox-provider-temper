package sandbox

import (
	"context"
	"testing"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

func TestReapDeletesExpiredAndIdleSandboxes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	expired := f.newSandbox(t, false)
	idle := f.newSandbox(t, false)
	active := f.newSandbox(t, false)
	f.pool.Exec(ctx, "UPDATE sandboxes SET expires_at = now() - interval '1 second' WHERE id = $1", expired.ID)
	f.pool.Exec(ctx, "UPDATE sandboxes SET last_activity_at = now() - interval '31 minutes' WHERE id = $1", idle.ID)

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
	if len(f.pods.deleted) != 2 {
		t.Fatalf("pods deleted = %v, want the two reaped ones", f.pods.deleted)
	}
}

// If the pod delete fails, the sandbox stays "deleting" and the reconcile
// finishes it later, keeping the reaper's reason.
func TestReapFailureIsFinishedByReconcileWithReason(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb := f.newSandbox(t, true)
	f.pool.Exec(ctx, "UPDATE sandboxes SET expires_at = now() - interval '1 second' WHERE id = $1", sb.ID)

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
