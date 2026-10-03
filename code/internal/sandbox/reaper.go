package sandbox

import (
	"context"
	"fmt"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// reapDeleteTimeout bounds each pod delete during a reap (a var so tests can
// shorten it).
var reapDeleteTimeout = 30 * time.Second

// Reap deletes every live sandbox that is past its TTL or has been idle
// (no exec, file transfer or SSH) longer than the idle timeout (D9). It
// returns how many it reaped.
//
// Claiming marks the rows "deleting" with the reason in one atomic UPDATE,
// so it's safe to run in every API replica. If a pod delete fails here, the
// row stays "deleting" and the status reconcile retries it, keeping the
// reason.
func (s *Service) Reap(ctx context.Context) (int, error) {
	idleReason := fmt.Sprintf("idle: no activity for %s", s.cfg.IdleTimeout)
	claimed, err := s.store.ClaimForReaping(ctx, s.cfg.IdleTimeout, idleReason)
	if err != nil {
		return 0, err
	}
	for _, sb := range claimed {
		s.ssh.Drop(sb.ID)
		// Bound each delete, so one hung Kubernetes call can't stall the
		// rest of the reap; the reconcile retries whatever times out.
		delCtx, cancel := context.WithTimeout(ctx, reapDeleteTimeout)
		err := s.pods.Delete(delCtx, sb.PodName)
		cancel()
		if err != nil {
			s.log.Error("reap: delete pod (reconcile will retry)", "sandbox", sb.ID, "err", err)
			continue
		}
		if _, err := s.store.MarkSandboxEnded(ctx, sb.ID, store.StatusDeleted, sb.StatusReason); err != nil {
			s.log.Error("reap: mark deleted", "sandbox", sb.ID, "err", err)
			continue
		}
		s.log.Info("sandbox reaped", "sandbox", sb.ID, "reason", sb.StatusReason)
	}
	return len(claimed), nil
}

// RunReaper reaps every interval until ctx ends.
func (s *Service) RunReaper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Reap(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("reap sandboxes", "err", err)
			}
		}
	}
}
