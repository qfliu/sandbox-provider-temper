package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
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
	f := &fakeSandboxSSHD{addr: ln.Addr().String()}
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

func newProxyFixture(t *testing.T) *proxyFixture {
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
	p := newProxyFixture(t)
	p.srv.statusInterval = 100 * time.Millisecond
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
