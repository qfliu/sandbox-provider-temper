package sandbox

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// PortDialer opens a connection to a port inside a sandbox.
type PortDialer func(ctx context.Context) (net.Conn, error)

// MaxPortRequests caps concurrent port requests per sandbox per API replica
// (review of #77): each holds an SSH channel with up to 2 MiB of window and
// a response being copied, so without a cap one user's own sandbox could
// make an API replica, shared by everyone, run out of memory. Exec is
// capped the same way by sshd's MaxSessions 64.
const MaxPortRequests = 64

// portActivityEvery is how often a long-lived port connection (a WebSocket,
// a stream) counts as activity, so the idle reaper doesn't end a sandbox
// that's in use; well under the 30-minute idle timeout.
const portActivityEvery = time.Minute

// ErrTooManyPortRequests: the sandbox already has MaxPortRequests in flight.
var ErrTooManyPortRequests = errors.New("too many concurrent port requests to this sandbox")

// portSlots counts in-flight port requests per sandbox.
type portSlots struct {
	mu sync.Mutex
	n  map[string]int
}

func (p *portSlots) take(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n == nil {
		p.n = map[string]int{}
	}
	if p.n[id] >= MaxPortRequests {
		return false
	}
	p.n[id]++
	return true
}

func (p *portSlots) give(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n[id]--; p.n[id] <= 0 {
		delete(p.n, id)
	}
}

// OpenPort gives access to a TCP port inside a running sandbox, for the
// HTTP proxy at /v1/sandboxes/{id}/ports/{port}/ (D66). Connections go over
// the sandbox's pooled, host-key-pinned SSH connection (D29) as SSH port
// forwards to 127.0.0.1:port inside the sandbox, so no network path to the
// sandbox opens beyond its sshd, and its sshd allows forwarding only to its
// own loopback. A server inside must listen on 127.0.0.1 or 0.0.0.0 (Docker's
// published ports do). The caller must call release when done; until then
// the sandbox counts as in use.
func (s *Service) OpenPort(ctx context.Context, user store.User, id string, port int) (dial PortDialer, release func(), err error) {
	if port < 1 || port > 65535 {
		return nil, nil, &ValidationError{"port must be between 1 and 65535"}
	}
	sb, err := s.Get(ctx, user, id)
	if err != nil {
		return nil, nil, err
	}
	if sb.Status != store.StatusRunning || sb.PodIP == "" {
		return nil, nil, &NotRunningError{Status: sb.Status}
	}
	if !s.ports.take(sb.ID) {
		return nil, nil, ErrTooManyPortRequests
	}
	client, releaseSSH, err := s.ssh.Get(ctx, sb.ID, net.JoinHostPort(sb.PodIP, "22"), sb.HostPublicKey)
	if err != nil {
		s.ports.give(sb.ID)
		s.log.Error("ssh connect", "sandbox", sb.ID, "err", err)
		return nil, nil, ErrUnreachable
	}
	touch := func() {
		if err := s.store.TouchSandbox(context.WithoutCancel(ctx), sb.ID); err != nil {
			s.log.Warn("record sandbox activity", "sandbox", sb.ID, "err", err)
		}
	}
	touch()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(portActivityEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				touch()
			}
		}
	}()
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(done)
			releaseSSH()
			s.ports.give(sb.ID)
		})
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	return func(ctx context.Context) (net.Conn, error) {
		conn, err := client.DialContext(ctx, "tcp", addr)
		var refused *ssh.OpenChannelError
		if err != nil && !errors.As(err, &refused) && ctx.Err() == nil {
			// Not "nothing listening" (sshd refused the channel) but the SSH
			// connection itself: drop it so the next request redials
			// (review of #77).
			s.ssh.Drop(sb.ID)
			s.log.Error("ssh connection lost", "sandbox", sb.ID, "err", err)
		}
		return conn, err
	}, release, nil
}
