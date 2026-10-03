package gateway

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// gotBanner reports whether the gateway answered a new TCP connection with
// its SSH banner (accepted) rather than closing it (over a limit).
func gotBanner(t *testing.T, addr string) (net.Conn, bool) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	return c, err == nil && strings.HasPrefix(line, "SSH-2.0-")
}

// One client IP can hold only so many connections at once, counted from
// accept (so unauthenticated handshakes count too); a slot frees up when a
// connection closes.
func TestPerIPConnectionLimit(t *testing.T) {
	srv := New(signer(t), noSandboxes{}, nil, quietLog())
	srv.SetLimits(0, 2, 0)
	addr := start(t, srv)
	a, okA := gotBanner(t, addr)
	b, okB := gotBanner(t, addr)
	defer b.Close()
	if !okA || !okB {
		t.Fatal("first two connections from one IP refused")
	}
	c, okC := gotBanner(t, addr)
	c.Close()
	if okC {
		t.Fatal("third connection from the same IP accepted; want it closed")
	}
	a.Close()
	time.Sleep(100 * time.Millisecond) // let the gateway notice the close
	d, okD := gotBanner(t, addr)
	defer d.Close()
	if !okD {
		t.Fatal("slot not freed after a connection closed")
	}
}

func TestTotalConnectionLimit(t *testing.T) {
	srv := New(signer(t), noSandboxes{}, nil, quietLog())
	srv.SetLimits(2, 100, 0)
	addr := start(t, srv)
	a, okA := gotBanner(t, addr)
	defer a.Close()
	b, okB := gotBanner(t, addr)
	defer b.Close()
	c, okC := gotBanner(t, addr)
	defer c.Close()
	if !okA || !okB || okC {
		t.Fatalf("accepted = %v %v %v; want the third refused at a total limit of 2", okA, okB, okC)
	}
}

// A sandbox takes only so many logged-in connections; the extra one is
// told why, and a slot frees up when a connection closes.
func TestPerSandboxConnectionLimit(t *testing.T) {
	p := newProxyFixture(t, func(s *Server) { s.SetLimits(0, 0, 1) })
	first := p.client(t)
	if _, err := first.NewSession(); err != nil {
		t.Fatalf("first connection: %v", err)
	}
	second := p.client(t) // login itself succeeds
	_, err := second.NewSession()
	if err == nil || !strings.Contains(err.Error(), "too many connections to this sandbox") {
		t.Fatalf("second connection's session: %v; want the per-sandbox limit message", err)
	}
	first.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		third := p.client(t)
		if s, err := third.NewSession(); err == nil {
			s.Close()
			return
		}
		third.Close()
		if time.Now().After(deadline) {
			t.Fatal("slot not freed after the first connection closed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
