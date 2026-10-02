package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/qfliu/sandbox-provider-temper/code/internal/k8s"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

const (
	// ReconcileInterval is how often the full database-vs-pods comparison runs.
	ReconcileInterval = 30 * time.Second
	// createGrace stops the reconcile from calling a brand-new sandbox lost
	// in the moment between inserting its row and its pod reaching the cache.
	createGrace = 30 * time.Second
	// eventTimeout bounds the database work for one pod event.
	eventTimeout = 10 * time.Second
)

// PodLister lists sandbox pods from the watcher's cache (faked in tests).
type PodLister interface {
	Pods() ([]*corev1.Pod, error)
}

// Syncer keeps sandbox statuses in the database in line with their pods.
//
// Live events give fast updates; the periodic reconcile catches anything
// missed (e.g. while the API was down) and removes orphan pods. Every update
// is conditional on the current status, so running it in several API
// replicas at once is harmless.
type Syncer struct {
	store *store.Store
	pods  Pods
	log   *slog.Logger
	cache PodLister
}

// NewSyncer returns a Syncer. Call SetCache before Reconcile.
func NewSyncer(s *store.Store, pods Pods, log *slog.Logger) *Syncer {
	return &Syncer{store: s, pods: pods, log: log}
}

// SetCache provides the pod cache that Reconcile compares against.
func (y *Syncer) SetCache(c PodLister) { y.cache = c }

// PodChanged handles a pod add/update event (k8s.PodHandler).
func (y *Syncer) PodChanged(pod *corev1.Pod) {
	ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
	defer cancel()
	y.apply(ctx, pod)
}

// PodGone handles a pod deletion event (k8s.PodHandler).
func (y *Syncer) PodGone(sandboxID string) {
	ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
	defer cancel()
	sb, err := y.store.GetSandbox(ctx, sandboxID)
	if err != nil {
		return // unknown or unreadable; reconcile will retry
	}
	y.podMissing(ctx, sb, false)
}

// apply moves one sandbox's status to match its pod.
func (y *Syncer) apply(ctx context.Context, pod *corev1.Pod) {
	id := k8s.SandboxID(pod)
	st := k8s.StateOf(pod)
	var err error
	switch st.Phase {
	case k8s.PodReady:
		err = y.store.MarkSandboxRunning(ctx, id, st.IP)
	case k8s.PodExpired:
		err = y.end(ctx, id, pod.Name, store.StatusDeleted, "expired: TTL reached (enforced by Kubernetes)")
	case k8s.PodFailed:
		err = y.end(ctx, id, pod.Name, store.StatusFailed, st.Reason)
	}
	if err != nil {
		y.log.Error("sync sandbox status", "sandbox", id, "err", err)
	}
}

// end records a final status, then removes the dead pod so it doesn't
// linger in the cluster.
func (y *Syncer) end(ctx context.Context, id, podName, status, reason string) error {
	sb, err := y.store.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	if store.IsLive(sb.Status) && sb.Status != store.StatusDeleting {
		if _, err := y.store.MarkSandboxEnded(ctx, id, status, reason); err != nil {
			return err
		}
		y.log.Info("sandbox ended", "sandbox", id, "status", status, "reason", reason)
	}
	return y.pods.Delete(ctx, podName)
}

// podMissing handles a live sandbox whose pod no longer exists.
func (y *Syncer) podMissing(ctx context.Context, sb store.Sandbox, fromReconcile bool) {
	var status, reason string
	switch {
	case sb.Status == store.StatusDeleting:
		if !fromReconcile {
			return // the delete request in progress records the final status
		}
		// The delete request didn't finish (e.g. the API restarted mid-way).
		status, reason = store.StatusDeleted, "deleted (completed by reconciler)"
	case store.IsLive(sb.Status):
		// A deletion event proves the pod existed and is gone. Only the
		// reconcile can't tell "gone" from "not created/cached yet".
		if fromReconcile && time.Since(sb.CreatedAt) < createGrace {
			return
		}
		status, reason = store.StatusLost, "pod disappeared (node failure or deleted outside the API)"
	default:
		return
	}
	if _, err := y.store.MarkSandboxEnded(ctx, sb.ID, status, reason); err != nil {
		y.log.Error("mark sandbox ended", "sandbox", sb.ID, "err", err)
		return
	}
	y.log.Info("sandbox ended", "sandbox", sb.ID, "status", status, "reason", reason)
}

// Reconcile compares every live sandbox with the pod cache once: it applies
// pod states, ends sandboxes whose pods are gone, and deletes pods that no
// live sandbox owns.
func (y *Syncer) Reconcile(ctx context.Context) error {
	live, err := y.store.ListLiveSandboxes(ctx)
	if err != nil {
		return err
	}
	pods, err := y.cache.Pods()
	if err != nil {
		return err
	}

	byID := make(map[string]*corev1.Pod, len(pods))
	for _, p := range pods {
		byID[k8s.SandboxID(p)] = p
	}
	liveIDs := make(map[string]bool, len(live))
	for _, sb := range live {
		liveIDs[sb.ID] = true
		if pod, ok := byID[sb.ID]; ok {
			if sb.Status == store.StatusDeleting && pod.DeletionTimestamp == nil {
				// A delete request marked the row but didn't get the pod
				// deleted (Kubernetes error, API restart): retry it.
				y.log.Info("retrying interrupted delete", "sandbox", sb.ID)
				if err := y.pods.Delete(ctx, pod.Name); err != nil {
					y.log.Error("retry pod delete", "sandbox", sb.ID, "err", err)
				}
				continue
			}
			y.apply(ctx, pod)
		} else {
			y.podMissing(ctx, sb, true)
		}
	}
	// isLive re-checks the database for anything not in our snapshot: a
	// sandbox created after we listed already has its Secret (and maybe its
	// pod), and deleting those would break it.
	isLive := stillLive(liveIDs, func(id string) (store.Sandbox, error) {
		return y.store.GetSandbox(ctx, id)
	}, y.log)
	for id, pod := range byID {
		if pod.DeletionTimestamp != nil || isLive(id) {
			continue
		}
		y.log.Info("deleting orphan pod", "pod", pod.Name)
		if err := y.pods.Delete(ctx, pod.Name); err != nil {
			y.log.Error("delete orphan pod", "pod", pod.Name, "err", err)
		}
	}
	deleted, err := y.pods.CleanupSecrets(ctx, isLive)
	for _, name := range deleted {
		y.log.Info("deleted orphan host key secret", "secret", name)
	}
	return err
}

// stillLive returns a check for "does this sandbox still need its pod and
// Secret?". It fails closed: only a definite answer from the database (no
// such row, or an ended status) counts as dead. Any other error, such as a
// timeout, counts as live, so a database blip can never make the reconcile
// delete a running sandbox's pod; the next pass retries.
func stillLive(snapshot map[string]bool, get func(id string) (store.Sandbox, error), log *slog.Logger) func(string) bool {
	return func(id string) bool {
		if snapshot[id] {
			return true
		}
		sb, err := get(id)
		if errors.Is(err, store.ErrNotFound) {
			return false
		}
		if err != nil {
			log.Warn("can't confirm sandbox is gone; keeping its resources", "sandbox", id, "err", err)
			return true
		}
		return store.IsLive(sb.Status)
	}
}

// Run reconciles immediately and then every ReconcileInterval until ctx ends.
func (y *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(ReconcileInterval)
	defer t.Stop()
	for {
		if err := y.Reconcile(ctx); err != nil && ctx.Err() == nil {
			y.log.Error("reconcile sandboxes", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
