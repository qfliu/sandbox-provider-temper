package sandbox

import (
	"context"
	"net"
	"strconv"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// PortDialer opens a connection to a port inside a sandbox.
type PortDialer func(ctx context.Context) (net.Conn, error)

// OpenPort gives access to a TCP port inside a running sandbox, for the
// HTTP proxy at /v1/sandboxes/{id}/ports/{port}/ (D66). Connections go over
// the sandbox's pooled, host-key-pinned SSH connection (D29) as SSH port
// forwards to 127.0.0.1:port inside the sandbox, so no network path to the
// sandbox opens beyond its sshd, and its sshd allows forwarding only to its
// own loopback. A server inside must listen on 127.0.0.1 or 0.0.0.0 (Docker's
// published ports do). The caller must call release when done.
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
	client, release, err := s.ssh.Get(ctx, sb.ID, net.JoinHostPort(sb.PodIP, "22"), sb.HostPublicKey)
	if err != nil {
		s.log.Error("ssh connect", "sandbox", sb.ID, "err", err)
		return nil, nil, ErrUnreachable
	}
	if err := s.store.TouchSandbox(ctx, sb.ID); err != nil {
		s.log.Warn("record sandbox activity", "sandbox", sb.ID, "err", err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	return func(ctx context.Context) (net.Conn, error) {
		return client.DialContext(ctx, "tcp", addr)
	}, release, nil
}
