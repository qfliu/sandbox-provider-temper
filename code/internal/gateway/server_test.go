package gateway

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

func signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// noSandboxes is a lookup where no sandbox exists.
type noSandboxes struct{}

func (noSandboxes) GetSandbox(context.Context, string) (store.Sandbox, error) {
	return store.Sandbox{}, store.ErrNotFound
}

func (noSandboxes) RecordSSHActivity(context.Context, string) error { return nil }

// serve runs srv on ln until the returned cancel is called; done receives
// Serve's result.
func serve(srv *Server, ln net.Listener) (cancel func(), done <-chan error) {
	ctx, c := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- srv.Serve(ctx, ln) }()
	return c, ch
}

// start runs a gateway on a random local port and returns its address.
func start(t *testing.T, srv *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cancel, done := serve(srv, ln)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return ln.Addr().String()
}

func dial(addr, user string, hostKey ssh.PublicKey, client ssh.Signer) error {
	c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(client)},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		Timeout:         5 * time.Second,
	})
	if err == nil {
		c.Close()
	}
	return err
}

func TestPresentsConfiguredHostKey(t *testing.T) {
	host := signer(t)
	addr := start(t, New(host, noSandboxes{}, nil, quietLog()))

	// Pinning the right host key gets past the handshake to authentication…
	err := dial(addr, "sb-0123456789ab", host.PublicKey(), signer(t))
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("err = %v; want an authentication failure (not a host key error)", err)
	}
	// …and pinning a different one fails at the host key.
	err = dial(addr, "sb-0123456789ab", signer(t).PublicKey(), signer(t))
	if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("err = %v; want host key mismatch", err)
	}
}

func TestSandboxIDPattern(t *testing.T) {
	for name, want := range map[string]bool{
		"sb-0123456789ab":   true,
		"sb-0123456789AB":   false, // ids are lowercase hex
		"sb-0123456789a":    false,
		"sb-0123456789abc":  false,
		"root":              false,
		"":                  false,
		"sb-0123456789ab\n": false,
	} {
		if got := sandboxIDPattern.MatchString(name); got != want {
			t.Errorf("%q: match = %v, want %v", name, got, want)
		}
	}
}

// readBanner returns once the server has sent its SSH version line, i.e.
// the connection is accepted and being handled.
func readBanner(t *testing.T, conn net.Conn) *bufio.Reader {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "SSH-2.0-") {
		t.Fatalf("no SSH banner: %q, %v", line, err)
	}
	return r
}

// waitClosed asserts the server closes the connection within limit.
func waitClosed(t *testing.T, r *bufio.Reader, conn net.Conn, limit time.Duration) {
	t.Helper()
	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(limit + 3*time.Second))
	for {
		if _, err := r.ReadByte(); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("connection still open after %s", time.Since(start))
			}
			if d := time.Since(start); d > limit {
				t.Fatalf("closed after %s, want within %s", d, limit)
			}
			return
		}
	}
}

// A client that connects and never speaks must be disconnected at the
// handshake deadline, not hold a connection forever.
func TestStalledClientIsDisconnected(t *testing.T) {
	srv := New(signer(t), noSandboxes{}, nil, quietLog())
	srv.handshakeTimeout = 300 * time.Millisecond
	conn, err := net.Dial("tcp", start(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := readBanner(t, conn)
	waitClosed(t, r, conn, 2*time.Second)
}

// Shutdown must close a connection that is open and being handled, and
// Serve must return promptly.
func TestShutdownClosesOpenConnections(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cancel, done := serve(New(signer(t), noSandboxes{}, nil, quietLog()), ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := readBanner(t, conn) // accepted and in the middle of its handshake

	cancel()
	waitClosed(t, r, conn, 2*time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve didn't return after shutdown")
	}
}

// A connection that is accepted after shutdown has started (the race where
// it misses the close-all sweep) must be closed, not tracked.
func TestConnectionArrivingDuringShutdownIsClosed(t *testing.T) {
	srv := New(signer(t), noSandboxes{}, nil, quietLog())
	srv.shutdown()

	client, server := net.Pipe()
	defer client.Close()
	if srv.track(server) {
		t.Fatal("connection tracked after shutdown started")
	}
	if _, err := server.Write([]byte("x")); err == nil {
		t.Fatal("connection arriving during shutdown was left open")
	}
}

// flakyListener fails Accept with a non-timeout error a few times before
// delegating, like a server out of file descriptors under a flood.
type flakyListener struct {
	net.Listener
	failures atomic.Int32
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return l.Listener.Accept()
}

// Accept errors other than shutdown must be retried, not stop the gateway.
func TestAcceptErrorsAreRetried(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := &flakyListener{Listener: inner}
	ln.failures.Store(3)
	host := signer(t)
	cancel, done := serve(New(host, noSandboxes{}, nil, quietLog()), ln)
	defer cancel()

	select { // give the 3 failures (5+10+20 ms backoff) time to happen
	case err := <-done:
		t.Fatalf("Serve exited on a temporary accept error: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// Still serving after 3 EMFILE errors: the handshake gets to auth.
	if err := dial(inner.Addr().String(), "sb-0123456789ab", host.PublicKey(), signer(t)); err == nil ||
		!strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("after accept errors: %v; want the gateway still serving", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
