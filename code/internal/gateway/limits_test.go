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

// One client can have only so many connections in their handshake at once
// (unauthenticated floods); a slot frees up when a connection closes.
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
	deadline := time.Now().Add(3 * time.Second) // poll: the gateway notices the close asynchronously
	for {
		d, okD := gotBanner(t, addr)
		d.Close()
		if okD {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("slot not freed after a connection closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Logged-in connections don't count toward the per-client handshake limit:
// users behind one NAT, or a tool opening many connections, aren't capped
// once they've authenticated (review of #50).
func TestLoggedInConnectionsLeaveTheHandshakeLimit(t *testing.T) {
	p := newProxyFixture(t, func(s *Server) { s.SetLimits(0, 1, 0) })
	first := p.client(t) // logged in: no longer counted
	if _, err := first.NewSession(); err != nil {
		t.Fatal(err)
	}
	second := p.client(t) // would be refused if logged-in connections counted
	if _, err := second.NewSession(); err != nil {
		t.Fatalf("second login from the same IP: %v", err)
	}
}

func TestClientKey(t *testing.T) {
	for addr, want := range map[string]string{
		"192.0.2.7:51234":                "192.0.2.7",
		"[2001:db8:1:2:aaaa::1]:22":      "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff:ffff::9]:22": "2001:db8:1:2::/64", // same /64, same client
		"[2001:db8:1:3::1]:22":           "2001:db8:1:3::/64",
		"[::ffff:192.0.2.7]:22":          "192.0.2.7", // IPv4-mapped counts as the IPv4 client
	} {
		a, _ := net.ResolveTCPAddr("tcp", addr)
		if got := clientKey(a); got != want {
			t.Errorf("clientKey(%s) = %q, want %q", addr, got, want)
		}
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
	if err == nil || !strings.Contains(err.Error(), "too many connections to this sandbox; close one and retry") {
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
