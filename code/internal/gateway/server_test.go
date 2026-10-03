package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
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

// start runs a gateway on a random local port and returns its address.
func start(t *testing.T, hostKey ssh.Signer) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(hostKey, slog.New(slog.NewTextHandler(io.Discard, nil))).Serve(ctx, ln) }()
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
	addr := start(t, host)

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

func TestRejectsEveryoneForNow(t *testing.T) {
	host := signer(t)
	addr := start(t, host)
	for _, user := range []string{"sb-0123456789ab", "root", "sb-XYZ", "user"} {
		if err := dial(addr, user, host.PublicKey(), signer(t)); err == nil {
			t.Errorf("user %q was let in; authentication isn't implemented until step 3.2", user)
		}
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

// A client that connects and never speaks must be disconnected (handshake
// deadline), not hold a connection forever.
func TestStalledClientIsDisconnected(t *testing.T) {
	old := handshakeTimeout
	t.Cleanup(func() { handshakeTimeout = old })
	handshakeTimeout = 300 * time.Millisecond

	addr := start(t, signer(t))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	conn.Read(buf) // server's version banner
	start := time.Now()
	for {
		if _, err := conn.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("stalled connection still open after %s", time.Since(start))
			}
			return // closed by the server: good
		}
	}
}

func TestShutdownClosesOpenConnections(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(signer(t), slog.New(slog.NewTextHandler(io.Discard, nil))).Serve(ctx, ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve didn't return after shutdown with a connection open")
	}
}
