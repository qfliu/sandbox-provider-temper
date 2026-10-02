package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/sshx"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

const (
	defaultExecTimeout = 60 * time.Second
	maxExecTimeout     = 10 * time.Minute
)

// NotRunningError means the sandbox exists but can't take commands yet (or
// any more), e.g. still pending or already deleted (HTTP 409).
type NotRunningError struct{ Status string }

func (e *NotRunningError) Error() string {
	return fmt.Sprintf("sandbox is %s, not running", e.Status)
}

// ErrUnreachable means the sandbox is running but SSH to it failed (HTTP 502).
var ErrUnreachable = errors.New("sandbox unreachable")

// ExecRequest is the body of POST /v1/sandboxes/{id}/exec.
type ExecRequest struct {
	Command string `json:"command"`
	// TimeoutSeconds is optional; 0 means 60 s. Max 600.
	TimeoutSeconds int `json:"timeout_seconds"`
	// Workdir is optional; relative paths start in /home/user.
	Workdir string `json:"workdir"`
}

// Exec runs a command in a running sandbox and returns its output.
func (s *Service) Exec(ctx context.Context, user store.User, id string, req ExecRequest) (sshx.ExecResult, error) {
	if req.Command == "" {
		return sshx.ExecResult{}, &ValidationError{"command is required"}
	}
	timeout := defaultExecTimeout
	if req.TimeoutSeconds != 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
		if timeout < time.Second || timeout > maxExecTimeout {
			return sshx.ExecResult{}, &ValidationError{fmt.Sprintf(
				"timeout_seconds must be between 1 and %d", int(maxExecTimeout/time.Second))}
		}
	}

	var res sshx.ExecResult
	err := s.withSSH(ctx, user, id, func(c *ssh.Client) error {
		var err error
		res, err = sshx.Run(ctx, c, req.Command, req.Workdir, timeout)
		return err
	})
	return res, err
}

// Upload writes r to path inside a running sandbox.
func (s *Service) Upload(ctx context.Context, user store.User, id, path string, r io.Reader) (written int64, err error) {
	if path == "" {
		return 0, &ValidationError{"path is required"}
	}
	err = s.withSSH(ctx, user, id, func(c *ssh.Client) error {
		written, err = sshx.Upload(c, path, r)
		return err
	})
	return written, err
}

// Download streams a file from a running sandbox into w.
func (s *Service) Download(ctx context.Context, user store.User, id, path string, setSize func(int64), w io.Writer) error {
	if path == "" {
		return &ValidationError{"path is required"}
	}
	return s.withSSH(ctx, user, id, func(c *ssh.Client) error {
		return sshx.Download(c, path, setSize, w)
	})
}

// withSSH checks the caller may use the sandbox and that it's running, gets
// a pooled host-key-pinned connection (D29), runs fn, and records activity.
// A connection that fails is dropped from the pool so the next call redials.
func (s *Service) withSSH(ctx context.Context, user store.User, id string, fn func(*ssh.Client) error) error {
	sb, err := s.Get(ctx, user, id)
	if err != nil {
		return err
	}
	if sb.Status != store.StatusRunning || sb.PodIP == "" {
		return &NotRunningError{Status: sb.Status}
	}

	client, release, err := s.ssh.Get(ctx, sb.ID, net.JoinHostPort(sb.PodIP, "22"), sb.HostPublicKey)
	if err != nil {
		s.log.Error("ssh connect", "sandbox", sb.ID, "err", err)
		return ErrUnreachable
	}
	defer release()
	if err := s.store.TouchSandbox(ctx, sb.ID); err != nil {
		s.log.Warn("record sandbox activity", "sandbox", sb.ID, "err", err)
	}

	err = fn(client)
	if err == nil {
		return nil
	}
	// Tell a dead connection apart from an ordinary failure (missing file,
	// permission denied, cancelled request) by pinging it. A command that
	// lost its exit status always counts as a lost connection: drop it so
	// the next call redials.
	_, _, pingErr := client.SendRequest("keepalive@openssh.com", true, nil)
	if pingErr != nil || errors.Is(err, sshx.ErrNoExitStatus) {
		s.ssh.Drop(sb.ID)
		s.log.Error("ssh connection lost", "sandbox", sb.ID, "err", err)
		return ErrUnreachable
	}
	var sftpErr *sftp.StatusError
	if errors.As(err, &sftpErr) {
		return &ValidationError{"file operation failed: " + sftpErr.Error()}
	}
	return err
}
