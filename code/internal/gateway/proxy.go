package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// Upstream opens the gateway's own SSH connection into a sandbox: with the
// platform-internal key, and only if the sandbox proves it holds its
// recorded host key (D7, D29). Implemented by sshx.Dialer.
type Upstream interface {
	Dial(ctx context.Context, addr, hostPublicKey string) (*ssh.Client, error)
}

// proxySessions connects an authenticated client to its sandbox and relays
// its session channels until either side goes away.
//
// Only "session" channels are relayed (shell, exec, pty, the sftp
// subsystem…). Port forwarding is refused: direct-tcpip would let a client
// reach the cluster network through the gateway.
func (s *Server) proxySessions(sconn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, id string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Global requests from the client: answer keepalives, refuse the rest
	// (tcpip-forward, i.e. ssh -R).
	go func() {
		for r := range reqs {
			r.Reply(r.Type == "keepalive@openssh.com", nil)
		}
	}()

	// Re-read the sandbox: its pod IP and host key, and whether it's still
	// running (it may have been deleted since authentication).
	lookupCtx, lookupCancel := context.WithTimeout(ctx, lookupTimeout)
	sb, err := s.sandboxes.GetSandbox(lookupCtx, id)
	lookupCancel()
	if err != nil || sb.Status != store.StatusRunning || sb.PodIP == "" {
		s.log.Warn("sandbox not available after login", "sandbox", id, "status", sb.Status, "err", err)
		rejectThenClose(sconn, chans, s.rejectGrace, ssh.ConnectionFailed, "sandbox is not running")
		return
	}
	up, err := s.upstream.Dial(ctx, net.JoinHostPort(sb.PodIP, s.sandboxPort), sb.HostPublicKey)
	if err != nil {
		s.log.Error("connect to sandbox", "sandbox", id, "err", err)
		rejectThenClose(sconn, chans, s.rejectGrace, ssh.ConnectionFailed, "sandbox unreachable, try again")
		return
	}
	defer up.Close()

	// Either side going away ends the whole connection.
	go func() { up.Wait(); sconn.Close() }()
	go s.watchSandbox(ctx, sconn, id)
	go s.keepalive(ctx, sconn)

	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.Prohibited, "only sessions are allowed (no port forwarding)")
			continue
		}
		go s.proxyChannel(nch, up, id)
	}
}

// proxyChannel relays one session channel between the client and the
// sandbox: stdin, stdout, stderr and channel requests both ways (pty-req,
// window-change, env, shell, exec, subsystem, signal; exit-status and
// exit-signal coming back).
func (s *Server) proxyChannel(nch ssh.NewChannel, up *ssh.Client, id string) {
	upCh, upReqs, err := up.OpenChannel(nch.ChannelType(), nch.ExtraData())
	if err != nil {
		var oce *ssh.OpenChannelError
		if errors.As(err, &oce) {
			nch.Reject(oce.Reason, oce.Message)
		} else {
			nch.Reject(ssh.ConnectionFailed, "sandbox unreachable")
		}
		return
	}
	downCh, downReqs, err := nch.Accept()
	if err != nil {
		upCh.Close()
		return
	}
	defer upCh.Close()
	defer downCh.Close()

	// Requests from the client go to the sandbox, with its answer relayed.
	// inflight tracks a request whose answer hasn't been relayed yet: the
	// channel must not be closed under it, or a fast command (one that
	// finishes before the "exec" reply is relayed) makes the client see the
	// channel vanish instead of its reply (EOF).
	var inflight sync.WaitGroup
	var reqMu sync.Mutex
	closed := false
	go func() {
		defer upCh.Close() // client closed its channel: end the sandbox side too (see below)
		for r := range downReqs {
			reqMu.Lock()
			if closed {
				reqMu.Unlock()
				if r.WantReply {
					r.Reply(false, nil)
				}
				continue
			}
			inflight.Add(1)
			reqMu.Unlock()
			ok, err := upCh.SendRequest(r.Type, r.WantReply, r.Payload)
			if r.WantReply {
				r.Reply(ok && err == nil, nil)
			}
			inflight.Done()
		}
	}()
	// Requests from the sandbox (exit-status, exit-signal…) go to the
	// client. upReqs closes once the sandbox closes the channel.
	upReqsDone := make(chan struct{})
	go func() {
		defer close(upReqsDone)
		for r := range upReqs {
			ok, _ := downCh.SendRequest(r.Type, r.WantReply, r.Payload)
			if r.WantReply {
				r.Reply(ok, nil)
			}
		}
	}()

	// Client stdin → sandbox; EOF from the client is passed on.
	go func() {
		io.Copy(upCh, downCh)
		upCh.CloseWrite()
	}()
	// Sandbox stdout and stderr → client. If the client can't take output any
	// more (it closed the session), close the sandbox side: otherwise nobody
	// reads it, the command blocks on stdout forever and these goroutines
	// leak until the whole connection drops (review finding on PR #26).
	// Closing the channel is what makes the sandbox's sshd end the command,
	// as OpenSSH does.
	var out sync.WaitGroup
	out.Add(2)
	go func() {
		defer out.Done()
		if _, err := io.Copy(downCh, upCh); err != nil {
			upCh.Close()
		}
	}()
	go func() {
		defer out.Done()
		if _, err := io.Copy(downCh.Stderr(), upCh.Stderr()); err != nil {
			upCh.Close()
		}
	}()
	out.Wait()
	downCh.CloseWrite()

	// Wait until the sandbox closed the channel, so its exit-status has been
	// relayed before we close the client's side; otherwise the client would
	// see no exit code. Then let any in-flight request get its answer.
	<-upReqsDone
	reqMu.Lock()
	closed = true
	reqMu.Unlock()
	inflight.Wait()
	s.log.Debug("session ended", "sandbox", id)
}

// watchSandbox closes the connection once the sandbox is no longer running
// (deleted, reaped, failed): a session must not outlive its sandbox.
func (s *Server) watchSandbox(ctx context.Context, sconn *ssh.ServerConn, id string) {
	t := time.NewTicker(s.statusInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
		sb, err := s.sandboxes.GetSandbox(lookupCtx, id)
		cancel()
		if errors.Is(err, store.ErrNotFound) || (err == nil && sb.Status != store.StatusRunning) {
			s.log.Info("sandbox ended; closing its SSH session", "sandbox", id, "status", sb.Status)
			sconn.Close()
			return
		}
		// Other errors (DB blip): keep the session; check again next tick.
	}
}

// keepalive pings the client so a vanished client (no FIN, e.g. a laptop
// that went to sleep) doesn't keep its sandbox connection open forever.
func (s *Server) keepalive(ctx context.Context, sconn *ssh.ServerConn) {
	t := time.NewTicker(s.keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := sconn.SendRequest("keepalive@openssh.com", true, nil)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				sconn.Close()
				return
			}
		case <-time.After(s.keepaliveInterval):
			s.log.Info("client stopped answering keepalives; closing", "remote", sconn.RemoteAddr().String())
			sconn.Close()
			return
		case <-ctx.Done():
			return
		}
	}
}

// rejectThenClose refuses every channel with msg and closes the connection
// after grace (enough for the client to receive the rejection), so a client
// can't hold an idle connection forever.
func rejectThenClose(sconn *ssh.ServerConn, chans <-chan ssh.NewChannel, grace time.Duration, reason ssh.RejectionReason, msg string) {
	timer := time.AfterFunc(grace, func() { sconn.Close() })
	defer timer.Stop()
	for nch := range chans {
		nch.Reject(reason, msg)
	}
}
