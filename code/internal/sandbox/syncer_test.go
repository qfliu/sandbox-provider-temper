package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/qfliu/sandbox-provider-temper/code/internal/k8s"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// fakeCache is a fixed list of pods standing in for the watcher's cache.
type fakeCache []*corev1.Pod

func (c fakeCache) Pods() ([]*corev1.Pod, error) { return c, nil }

func sandboxPod(id string, mut func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: id, Labels: map[string]string{k8s.LabelSandboxID: id}}}
	p.Status.Phase = corev1.PodPending
	if mut != nil {
		mut(p)
	}
	return p
}

func readyPod(p *corev1.Pod) {
	p.Status.Phase = corev1.PodRunning
	p.Status.PodIP = "10.244.0.9"
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
}

func (f fixture) syncer() *Syncer {
	return NewSyncer(f.st, f.pods, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// newSandbox creates a sandbox for alice, optionally backdated so the
// syncer's create grace period doesn't apply.
func (f fixture) newSandbox(t *testing.T, old bool) store.Sandbox {
	t.Helper()
	ctx := context.Background()
	sb, err := f.svc.Create(ctx, f.alice, CreateRequest{Image: "ubuntu-base", SSHPublicKey: validKey})
	if err != nil {
		t.Fatal(err)
	}
	if old {
		if _, err := f.pool.Exec(ctx, "UPDATE sandboxes SET created_at = now() - interval '5 minutes' WHERE id = $1", sb.ID); err != nil {
			t.Fatal(err)
		}
	}
	return sb
}

func (f fixture) status(t *testing.T, id string) store.Sandbox {
	t.Helper()
	sb, err := f.st.GetSandbox(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestReadyPodMarksRunning(t *testing.T) {
	f := newFixture(t)
	sb := f.newSandbox(t, false)

	f.syncer().PodChanged(sandboxPod(sb.ID, readyPod))

	got := f.status(t, sb.ID)
	if got.Status != store.StatusRunning || got.PodIP != "10.244.0.9" {
		t.Fatalf("got status %q ip %q, want running at 10.244.0.9", got.Status, got.PodIP)
	}
}

func TestExpiredAndFailedPodsEndTheSandbox(t *testing.T) {
	f := newFixture(t)
	expired := f.newSandbox(t, false)
	broken := f.newSandbox(t, false)
	y := f.syncer()

	y.PodChanged(sandboxPod(expired.ID, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed; p.Status.Reason = "DeadlineExceeded" }))
	y.PodChanged(sandboxPod(broken.ID, func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
	}))

	if got := f.status(t, expired.ID); got.Status != store.StatusDeleted || got.StatusReason != "expired: TTL reached (enforced by Kubernetes)" {
		t.Fatalf("expired: %q / %q", got.Status, got.StatusReason)
	}
	if got := f.status(t, broken.ID); got.Status != store.StatusFailed || got.StatusReason == "" {
		t.Fatalf("broken: %q / %q", got.Status, got.StatusReason)
	}
	if len(f.pods.deleted) != 2 {
		t.Fatalf("dead pods deleted = %v, want both", f.pods.deleted)
	}
}

func TestPodGone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	lostOne := f.newSandbox(t, true)
	brandNew := f.newSandbox(t, false) // a deletion event is final, even for a new sandbox
	beingDeleted := f.newSandbox(t, true)
	f.st.MarkSandboxDeleting(ctx, beingDeleted.ID)
	y := f.syncer()

	y.PodGone(lostOne.ID)
	y.PodGone(brandNew.ID)
	y.PodGone(beingDeleted.ID)

	if got := f.status(t, lostOne.ID); got.Status != store.StatusLost {
		t.Fatalf("pod vanished: status %q, want lost", got.Status)
	}
	if got := f.status(t, brandNew.ID); got.Status != store.StatusLost {
		t.Fatalf("brand-new sandbox's pod deleted: status %q, want lost", got.Status)
	}
	if got := f.status(t, beingDeleted.ID); got.Status != store.StatusDeleting {
		t.Fatalf("intentional delete: status %q, want deleting (not lost)", got.Status)
	}
}

// A user delete must end as "deleted", never "lost", even though the
// watcher sees the pod disappear in the middle of it.
func TestDeleteIsNotMistakenForLost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb := f.newSandbox(t, true)
	y := f.syncer()
	f.pods.onDelete = func(string) { y.PodGone(sb.ID) } // watcher event arrives mid-delete

	got, err := f.svc.Delete(ctx, f.alice, sb.ID)
	if err != nil || got.Status != store.StatusDeleted || got.StatusReason != "deleted by owner" {
		t.Fatalf("delete = %q / %q, %v", got.Status, got.StatusReason, err)
	}
}

func TestReconcile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	running := f.newSandbox(t, true)  // pod exists and is ready
	vanished := f.newSandbox(t, true) // pod missing → lost
	stuck := f.newSandbox(t, true)    // delete interrupted → deleted
	f.st.MarkSandboxDeleting(ctx, stuck.ID)
	justCreated := f.newSandbox(t, false) // pod not in the cache yet → left alone (grace period)
	ended := f.newSandbox(t, true)        // already deleted, but its pod survived
	f.st.MarkSandboxEnded(ctx, ended.ID, store.StatusDeleted, "earlier")

	y := f.syncer()
	y.SetCache(fakeCache{
		sandboxPod(running.ID, readyPod),
		sandboxPod(ended.ID, readyPod),
		sandboxPod("sb-nobodyknows", readyPod),
	})
	if err := y.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]string{
		running.ID:     store.StatusRunning,
		vanished.ID:    store.StatusLost,
		justCreated.ID: store.StatusPending,
		stuck.ID:       store.StatusDeleted,
		ended.ID:       store.StatusDeleted,
	} {
		if got := f.status(t, id).Status; got != want {
			t.Errorf("%s: status %q, want %q", id, got, want)
		}
	}
	deleted := map[string]bool{}
	for _, name := range f.pods.deleted {
		deleted[name] = true
	}
	if !deleted[ended.ID] || !deleted["sb-nobodyknows"] || deleted[running.ID] {
		t.Fatalf("orphan cleanup deleted %v; want the ended sandbox's pod and the unknown pod only", f.pods.deleted)
	}
}

func TestReconcileCleansSecretsButNotNewSandboxes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	live := f.newSandbox(t, true)
	ended := f.newSandbox(t, true)
	f.st.MarkSandboxEnded(ctx, ended.ID, store.StatusDeleted, "earlier")

	y := f.syncer()
	y.SetCache(fakeCache{})
	// A sandbox created after the reconcile lists live rows must keep its
	// Secret: simulate it by creating one while CleanupSecrets runs.
	var late store.Sandbox
	f.pods.secrets = []string{live.ID, ended.ID}
	y.pods = cleanupHook{f.pods, func() {
		late = f.newSandbox(t, false)
		f.pods.secrets = append(f.pods.secrets, late.ID)
	}}
	if err := y.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.pods.cleanedSecrets) != 1 || f.pods.cleanedSecrets[0] != ended.ID+"-hostkey" {
		t.Fatalf("cleaned %v, want only the ended sandbox's secret", f.pods.cleanedSecrets)
	}
}

// cleanupHook runs before() just before delegating CleanupSecrets.
type cleanupHook struct {
	*fakePods
	before func()
}

func (c cleanupHook) CleanupSecrets(ctx context.Context, isLive func(string) bool) ([]string, error) {
	c.before()
	return c.fakePods.CleanupSecrets(ctx, isLive)
}

func TestStillLiveFailsClosed(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rows := map[string]store.Sandbox{
		"sb-running": {Status: store.StatusRunning},
		"sb-deleted": {Status: store.StatusDeleted},
	}
	get := func(id string) (store.Sandbox, error) {
		if id == "sb-dberror" {
			return store.Sandbox{}, errors.New("connection reset by peer")
		}
		if sb, ok := rows[id]; ok {
			return sb, nil
		}
		return store.Sandbox{}, store.ErrNotFound
	}
	isLive := stillLive(map[string]bool{"sb-snapshot": true}, get, log)

	for id, want := range map[string]bool{
		"sb-snapshot": true,  // in the listed snapshot
		"sb-running":  true,  // created after the snapshot
		"sb-deleted":  false, // definitely ended
		"sb-missing":  false, // no such row
		"sb-dberror":  true,  // unknown: must NOT be treated as dead
	} {
		if got := isLive(id); got != want {
			t.Errorf("isLive(%s) = %v, want %v", id, got, want)
		}
	}
}

// A delete whose pod deletion failed stays "deleting" (503 to the caller),
// and the next reconcile deletes the pod; once it's gone the row ends.
func TestInterruptedDeleteIsRetried(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb := f.newSandbox(t, true)

	f.pods.failDelete = true
	if _, err := f.svc.Delete(ctx, f.alice, sb.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("delete with failing Kubernetes: err = %v, want ErrUnavailable", err)
	}
	if got := f.status(t, sb.ID).Status; got != store.StatusDeleting {
		t.Fatalf("status %q, want deleting", got)
	}

	f.pods.failDelete = false
	y := f.syncer()
	y.SetCache(fakeCache{sandboxPod(sb.ID, readyPod)}) // pod still running
	if err := y.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.pods.deleted) != 1 || f.pods.deleted[0] != sb.ID {
		t.Fatalf("reconcile should retry the pod delete, deleted = %v", f.pods.deleted)
	}

	y.SetCache(fakeCache{}) // pod now gone
	if err := y.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.status(t, sb.ID).Status; got != store.StatusDeleted {
		t.Fatalf("after the pod is gone: status %q, want deleted", got)
	}
}

func TestReconcileDoesNotRedeleteTerminatingPod(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb := f.newSandbox(t, true)
	f.st.MarkSandboxDeleting(ctx, sb.ID)

	now := metav1.Now()
	y := f.syncer()
	y.SetCache(fakeCache{sandboxPod(sb.ID, func(p *corev1.Pod) { p.DeletionTimestamp = &now })})
	y.Reconcile(ctx)
	if len(f.pods.deleted) != 0 {
		t.Fatalf("pod already terminating was deleted again: %v", f.pods.deleted)
	}
}
