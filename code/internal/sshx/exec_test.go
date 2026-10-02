package sshx

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// connect starts a test server with onExec and returns a pinned client.
func connect(t *testing.T, onExec func(string) execReply) (*ssh.Client, *testServer) {
	t.Helper()
	signer := newClientSigner(t)
	hostPEM, hostPub, _ := GenerateHostKey()
	srv := startServer(t, hostPEM, signer.PublicKey(), onExec)
	c, err := Dialer{Signer: signer}.Dial(context.Background(), srv.addr, hostPub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, srv
}

func TestRunReturnsOutputAndExitCode(t *testing.T) {
	c, srv := connect(t, func(string) execReply {
		return execReply{stdout: "out", stderr: "err", code: 3}
	})
	res, err := Run(context.Background(), c, "make test", "proj", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out" || res.Stderr != "err" || res.ExitCode != 3 || res.TimedOut || res.Truncated {
		t.Fatalf("unexpected result %+v", res)
	}
	// The command is run in the workdir, under the sandbox's `timeout`.
	if got, want := srv.lastCmd.Load().(string), "cd 'proj' && exec timeout -k 5 60 bash -c 'make test'"; got != want {
		t.Fatalf("remote command = %q\nwant %q", got, want)
	}
}

func TestRunMissingExitStatus(t *testing.T) {
	// The session ended without an exit status well before the limit (sshd
	// or the connection died): an error, not a fake "exit -1" result.
	c, _ := connect(t, func(string) execReply { return execReply{noExitStatus: true} })
	res, err := Run(context.Background(), c, "sleep 20", "", time.Minute)
	if !errors.Is(err, ErrNoExitStatus) {
		t.Fatalf("got %+v, %v; want ErrNoExitStatus", res, err)
	}
}

func TestRunTruncatesHugeOutput(t *testing.T) {
	c, _ := connect(t, func(string) execReply {
		return execReply{stdout: strings.Repeat("y", MaxOutput+100)}
	})
	res, err := Run(context.Background(), c, "yes", "", time.Minute)
	if err != nil || !res.Truncated || len(res.Stdout) != MaxOutput {
		t.Fatalf("truncated=%v len=%d err=%v", res.Truncated, len(res.Stdout), err)
	}
}

func TestWrapCommandQuoting(t *testing.T) {
	got := wrapCommand(`echo 'hi' "$HOME"; rm -rf /tmp/x`, "my dir", 1500*time.Millisecond)
	want := `cd 'my dir' && exec timeout -k 5 1 bash -c 'echo '\''hi'\'' "$HOME"; rm -rf /tmp/x'`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 5}
	b.Write([]byte("abc"))
	if n, _ := b.Write([]byte("defg")); n != 4 {
		t.Fatal("writer must report all bytes taken so the command isn't blocked")
	}
	if b.String() != "abcde" || !b.truncated {
		t.Fatalf("got %q truncated=%v", b.String(), b.truncated)
	}
}

func TestUploadAndDownload(t *testing.T) {
	c, _ := connect(t, nil)

	n, err := Upload(c, "proj/sub/app.py", strings.NewReader("print(1)\n"))
	if err != nil || n != 9 {
		t.Fatalf("upload = %d, %v", n, err)
	}
	var size int64
	var out bytes.Buffer
	if err := Download(c, "/home/user/proj/sub/app.py", func(s int64) { size = s }, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "print(1)\n" || size != 9 {
		t.Fatalf("download = %q (size %d)", out.String(), size)
	}
	if err := Download(c, "nope.txt", func(int64) {}, &out); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: err = %v, want ErrNotFound", err)
	}
	if err := Download(c, "proj", func(int64) {}, &out); !errors.Is(err, ErrIsDir) {
		t.Fatalf("directory: err = %v, want ErrIsDir", err)
	}
}

func TestResolvePath(t *testing.T) {
	for in, want := range map[string]string{
		"a.txt":         "/home/user/a.txt",
		"proj/../b.txt": "/home/user/b.txt",
		"/tmp/x":        "/tmp/x",
		"/tmp/../etc":   "/etc",
	} {
		if got := ResolvePath(in); got != want {
			t.Errorf("ResolvePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPoolReusesAndRedials(t *testing.T) {
	signer := newClientSigner(t)
	hostPEM, hostPub, _ := GenerateHostKey()
	srv := startServer(t, hostPEM, signer.PublicKey(), nil)
	p := NewPool(Dialer{Signer: signer})
	ctx := context.Background()

	a, relA, err := p.Get(ctx, "sb-1", srv.addr, hostPub)
	if err != nil {
		t.Fatal(err)
	}
	relA()
	b, relB, _ := p.Get(ctx, "sb-1", srv.addr, hostPub)
	relB()
	if a != b || srv.dials.Load() != 1 {
		t.Fatalf("second Get should reuse the connection (dials=%d)", srv.dials.Load())
	}

	p.Drop("sb-1")
	if _, _, err := p.Get(ctx, "sb-1", srv.addr, hostPub); err != nil || srv.dials.Load() != 2 {
		t.Fatalf("after Drop, Get should redial (dials=%d, err=%v)", srv.dials.Load(), err)
	}

	// A different recorded host key for the same id (the sandbox was
	// replaced) must not reuse the old connection, and must be verified.
	_, otherPub, _ := GenerateHostKey()
	if _, _, err := p.Get(ctx, "sb-1", srv.addr, otherPub); err == nil {
		t.Fatal("pool reused or accepted a connection with the wrong host key")
	}
	if p.Len() != 0 {
		t.Fatalf("stale connection kept: len=%d", p.Len())
	}
}

// CloseIdle must never close a connection a request is still using, even if
// that request has outlasted the idle timeout (review finding H2).
func TestCloseIdleSkipsConnectionsInUse(t *testing.T) {
	signer := newClientSigner(t)
	hostPEM, hostPub, _ := GenerateHostKey()
	srv := startServer(t, hostPEM, signer.PublicKey(), nil)
	p := NewPool(Dialer{Signer: signer})
	p.idle = 50 * time.Millisecond

	c, release, err := p.Get(context.Background(), "sb-1", srv.addr, hostPub)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // a "long exec", past the idle timeout
	p.CloseIdle()
	if p.Len() != 1 {
		t.Fatal("connection in use was closed as idle")
	}
	if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("in-use connection is dead: %v", err)
	}

	release()
	release() // releasing twice must not go negative
	p.CloseIdle()
	if p.Len() != 1 {
		t.Fatal("idle time must count from release, not from Get")
	}
	time.Sleep(100 * time.Millisecond)
	p.CloseIdle()
	if p.Len() != 0 {
		t.Fatal("released connection idle past the timeout should be closed")
	}
}

func TestRunTimedOutDetection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     uint32
		slow     bool // the command ran past the 1 s limit
		timedOut bool
	}{
		{"timeout via SIGTERM (124)", 124, true, true},
		{"timeout escalated to SIGKILL (137)", 137, true, true},
		{"command itself exits 124 early", 124, false, false},
		{"OOM-killed early (137)", 137, false, false},
		{"slow but normal failure", 1, true, false},
	} {
		c, _ := connect(t, func(string) execReply {
			if tc.slow {
				time.Sleep(1100 * time.Millisecond)
			}
			return execReply{code: tc.code}
		})
		res, err := Run(context.Background(), c, "x", "", time.Second)
		if err != nil || res.TimedOut != tc.timedOut || res.ExitCode != int(tc.code) {
			t.Errorf("%s: got %+v, %v; want timed_out=%v", tc.name, res, err, tc.timedOut)
		}
	}
}
