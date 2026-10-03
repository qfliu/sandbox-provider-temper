package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/sshx"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// fakeSandboxSSHD stands in for a sandbox's sshd: it presents hostKey,
// accepts only internalKey, and records the channel requests it receives.
// exec "fail N" exits with status N after writing to stdout and stderr; any
// other exec echoes the command; "shell" (after pty-req) echoes stdin back;
// subsystem "sftp" serves an in-memory filesystem.
type fakeSandboxSSHD struct {
	addr     string
	mu       sync.Mutex
	requests []string
	// foreverStopped is closed when an exec "forever" command stops writing
	// because its channel was closed (like sshd killing the command).
	foreverStopped chan struct{}
}

func (f *fakeSandboxSSHD) saw(r string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
}

func (f *fakeSandboxSSHD) seen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.requests, ",")
}

func startSandboxSSHD(t *testing.T, hostKey ssh.Signer, internalKey ssh.PublicKey) *fakeSandboxSSHD {
	t.Helper()
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(k.Marshal(), internalKey.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("not the internal key")
	}}
	cfg.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSandboxSSHD{addr: ln.Addr().String(), foreverStopped: make(chan struct{})}
	fs := sftp.InMemHandler()
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					ch, creqs, err := nch.Accept()
					if err != nil {
						continue
					}
					go f.serve(ch, creqs, fs)
				}
			}()
		}
	}()
	return f
}

func (f *fakeSandboxSSHD) serve(ch ssh.Channel, reqs <-chan *ssh.Request, fs sftp.Handlers) {
	defer ch.Close()
	exit := func(code uint32) {
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{code}))
	}
	for r := range reqs {
		f.saw(r.Type)
		switch r.Type {
		case "pty-req", "window-change", "env":
			r.Reply(r.WantReply, nil)
		case "exec":
			var p struct{ Cmd string }
			ssh.Unmarshal(r.Payload, &p)
			r.Reply(true, nil)
			if p.Cmd == "forever" { // like `yes`: writes until the channel goes away
				go func() {
					for {
						if _, err := io.WriteString(ch, "y\n"); err != nil {
							close(f.foreverStopped)
							return
						}
					}
				}()
				continue
			}
			var code uint32
			if n, _ := fmt.Sscanf(p.Cmd, "fail %d", &code); n == 1 {
				io.WriteString(ch, "out\n")
				io.WriteString(ch.Stderr(), "err\n")
			} else {
				io.WriteString(ch, "ran: "+p.Cmd+"\n")
			}
			ch.CloseWrite()
			exit(code)
			return
		case "shell":
			r.Reply(true, nil)
			go func() { // echo until the client closes stdin, still reading requests
				io.Copy(ch, ch)
				ch.CloseWrite()
				exit(0)
				ch.Close()
			}()
		case "subsystem":
			r.Reply(true, nil)
			srv := sftp.NewRequestServer(ch, fs)
			srv.Serve()
			srv.Close()
			return
		default:
			r.Reply(false, nil)
		}
	}
}

// proxyFixture: a gateway in front of one running sandbox whose sshd is
// fake; the client logs in with owner.
type proxyFixture struct {
	gw      string
	host    ssh.PublicKey
	owner   ssh.Signer
	sshd    *fakeSandboxSSHD
	rows    *fakeSandboxes
	srv     *Server
	sandbox string
}

// newProxyFixture starts the gateway after applying opts, so tests never
// change a server setting while its goroutines may be reading it.
func newProxyFixture(t *testing.T, opts ...func(*Server)) *proxyFixture {
	t.Helper()
	gwHost, owner, internal := signer(t), signer(t), signer(t)
	sbHostPEM, sbHostPub, _ := sshx.GenerateHostKey()
	sbHost, _ := ssh.ParsePrivateKey(sbHostPEM)
	sshd := startSandboxSSHD(t, sbHost, internal.PublicKey())
	ip, port, _ := net.SplitHostPort(sshd.addr)

	const id = "sb-00000000000a"
	rows := &fakeSandboxes{rows: map[string]store.Sandbox{id: {
		ID: id, Status: store.StatusRunning, PodIP: ip, HostPublicKey: sbHostPub,
		SSHPublicKey: authorized(owner.PublicKey()),
	}}}
	srv := New(gwHost, rows, sshx.Dialer{Signer: internal}, quietLog())
	srv.sandboxPort = port
	for _, o := range opts {
		o(srv)
	}
	return &proxyFixture{gw: start(t, srv), host: gwHost.PublicKey(), owner: owner, sshd: sshd, rows: rows, srv: srv, sandbox: id}
}

func (p *proxyFixture) client(t *testing.T) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", p.gw, &ssh.ClientConfig{
		User: p.sandbox, Auth: []ssh.AuthMethod{ssh.PublicKeys(p.owner)},
		HostKeyCallback: ssh.FixedHostKey(p.host), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestProxyExecRelaysOutputAndExitCode(t *testing.T) {
	p := newProxyFixture(t)
	sess, _ := p.client(t).NewSession()
	var out, errOut bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errOut
	err := sess.Run("fail 7")
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 7 {
		t.Fatalf("exit = %v, want exit status 7", err)
	}
	if out.String() != "out\n" || errOut.String() != "err\n" {
		t.Fatalf("stdout %q stderr %q; want them kept apart", out.String(), errOut.String())
	}

	sess2, _ := p.client(t).NewSession()
	got, err := sess2.Output("uname -r")
	if err != nil || string(got) != "ran: uname -r\n" {
		t.Fatalf("exec = %q, %v", got, err)
	}
}

func TestProxyInteractiveShellWithPTY(t *testing.T) {
	p := newProxyFixture(t)
	sess, _ := p.client(t).NewSession()
	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, _ := sess.StdinPipe()
	var out bytes.Buffer
	sess.Stdout = &out
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	sess.WindowChange(40, 120)
	io.WriteString(stdin, "hello\n")
	stdin.Close()
	if err := sess.Wait(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\n" {
		t.Fatalf("shell echo = %q", out.String())
	}
	if seen := p.sshd.seen(); !strings.Contains(seen, "pty-req") || !strings.Contains(seen, "window-change") {
		t.Fatalf("sandbox saw %q; pty-req and window-change must be relayed", seen)
	}
}

func TestProxySFTP(t *testing.T) {
	p := newProxyFixture(t)
	c, err := sftp.NewClient(p.client(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, _ := c.Create("/notes.txt")
	f.Write([]byte("via the gateway"))
	f.Close()
	r, err := c.Open("/notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "via the gateway" {
		t.Fatalf("sftp round trip = %q", got)
	}
}

// Port forwarding through the gateway would reach the cluster network.
func TestProxyRefusesPortForwarding(t *testing.T) {
	p := newProxyFixture(t)
	c := p.client(t)
	if _, err := c.Dial("tcp", "10.96.0.1:443"); err == nil {
		t.Fatal("direct-tcpip (ssh -L) was allowed")
	}
	if _, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Fatal("tcpip-forward (ssh -R) was allowed")
	}
}

// A session must not outlive its sandbox.
func TestProxyClosesWhenSandboxEnds(t *testing.T) {
	p := newProxyFixture(t, func(s *Server) { s.statusInterval = 100 * time.Millisecond })
	c := p.client(t)
	sess, _ := c.NewSession()
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	p.rows.mu.Lock()
	sb := p.rows.rows[p.sandbox]
	sb.Status = store.StatusDeleted
	p.rows.rows[p.sandbox] = sb
	p.rows.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session still open 3s after its sandbox was deleted")
	}
}

// If the sandbox's host key doesn't match the recorded one (e.g. a reused
// pod IP), the gateway must not connect the client to it.
func TestProxyRefusesWrongSandboxHostKey(t *testing.T) {
	p := newProxyFixture(t)
	_, otherPub, _ := sshx.GenerateHostKey()
	p.rows.mu.Lock()
	sb := p.rows.rows[p.sandbox]
	sb.HostPublicKey = otherPub
	p.rows.rows[p.sandbox] = sb
	p.rows.mu.Unlock()

	if _, err := p.client(t).NewSession(); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("session = %v; want refusal (sandbox host key mismatch)", err)
	}
}

// Closing one session while keeping the connection must end the command
// in the sandbox (review finding on PR #26: the gateway only half-closed
// the sandbox side, so the command blocked on stdout forever and the
// relay goroutines leaked until the whole connection dropped).
func TestProxyClosingSessionEndsSandboxCommand(t *testing.T) {
	p := newProxyFixture(t)
	c := p.client(t) // the connection stays open throughout
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, _ := sess.StdoutPipe()
	if err := sess.Start("forever"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := io.ReadFull(out, buf); err != nil { // it's running
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()
	sess.Close()

	select {
	case <-p.sshd.foreverStopped:
	case <-time.After(3 * time.Second):
		t.Fatal("sandbox command still running 3s after the client closed its session")
	}
	// The relay goroutines for that session are gone too.
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() >= before && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n >= before {
		t.Fatalf("goroutines %d → %d after closing the session; relay goroutines leaked", before, n)
	}
	// The connection is still usable for new sessions.
	s2, _ := c.NewSession()
	if got, err := s2.Output("after"); err != nil || string(got) != "ran: after\n" {
		t.Fatalf("new session on the same connection: %q, %v", got, err)
	}
}

// A connection whose sandbox isn't available must not stay open forever.
func TestUnavailableSandboxConnectionIsClosed(t *testing.T) {
	p := newProxyFixture(t, func(s *Server) { s.rejectGrace = 200 * time.Millisecond })
	p.rows.mu.Lock()
	sb := p.rows.rows[p.sandbox]
	sb.PodIP = "" // e.g. the pod is gone
	p.rows.rows[p.sandbox] = sb
	p.rows.mu.Unlock()

	// Login succeeds only if running with an IP; force the post-login path by
	// keeping it running but without a reachable pod.
	c := p.client(t)
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection to an unavailable sandbox still open after the grace period")
	}
}

// openShell opens a session with stdin held open (with no stdin, Go's
// client sends EOF at once and the fake shell would exit immediately).
func openShell(t *testing.T, c *ssh.Client) *ssh.Session {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	return sess
}

// waitStable waits until touches stop changing, then returns the count.
func waitStable(rows *fakeSandboxes) int32 {
	prev := rows.touches.Load()
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		n := rows.touches.Load()
		if n == prev {
			return n
		}
		prev = n
	}
	return prev
}

func fastTouches(s *Server) {
	s.touchInterval = 50 * time.Millisecond
	s.touchMinGap = 10 * time.Millisecond
}

// Open SSH sessions count as activity, so the idle reaper doesn't delete a
// sandbox someone is working in over SSH (batch review of #22–#26).
func TestOpenSessionRecordsActivity(t *testing.T) {
	p := newProxyFixture(t, fastTouches)
	c := p.client(t)
	time.Sleep(200 * time.Millisecond) // several ticks with no session
	if n := p.rows.touches.Load(); n != 0 {
		t.Fatalf("%d touches with no session open; a bare connection shouldn't count", n)
	}

	sess := openShell(t, c)
	time.Sleep(300 * time.Millisecond)
	if during := p.rows.touches.Load(); during < 3 {
		t.Fatalf("%d touches during an open session of ~300ms (interval 50ms), want several", during)
	}
	sess.Close()
	after := waitStable(p.rows)
	time.Sleep(300 * time.Millisecond)
	if n := p.rows.touches.Load(); n != after {
		t.Fatalf("touches kept going after the session closed (%d → %d)", after, n)
	}
}

// The open-session count must handle several sessions on one connection.
func TestTwoSessionsOneCloses(t *testing.T) {
	p := newProxyFixture(t, fastTouches)
	c := p.client(t)
	a, b := openShell(t, c), openShell(t, c)
	time.Sleep(150 * time.Millisecond)

	a.Close()
	time.Sleep(100 * time.Millisecond)
	mid := p.rows.touches.Load()
	time.Sleep(300 * time.Millisecond)
	if n := p.rows.touches.Load(); n <= mid {
		t.Fatalf("touches stopped (%d → %d) while one session was still open", mid, n)
	}

	b.Close()
	after := waitStable(p.rows)
	time.Sleep(300 * time.Millisecond)
	if n := p.rows.touches.Load(); n != after {
		t.Fatalf("touches continued after both sessions closed (%d → %d)", after, n)
	}
}

// Opening a session must not wait on the database (review finding on #27).
func TestSessionOpenDoesNotWaitOnDatabase(t *testing.T) {
	p := newProxyFixture(t)
	p.rows.touchDelay = 3 * time.Second
	c := p.client(t)
	start := time.Now()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("hello")
	if err != nil || string(out) != "ran: hello\n" {
		t.Fatalf("exec = %q, %v", out, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("session took %s with a 3s-slow database; it waited on the touch", d)
	}
}

// Many sessions opened at once cause at most one touch per touchMinGap.
func TestManySessionsAreThrottled(t *testing.T) {
	p := newProxyFixture(t) // default: touch at most every 10 s
	c := p.client(t)
	for i := 0; i < 20; i++ {
		s, _ := c.NewSession()
		s.Output("x")
	}
	if n := waitStable(p.rows); n != 1 {
		t.Fatalf("%d touches for 20 quick sessions, want 1 (throttled)", n)
	}
}
