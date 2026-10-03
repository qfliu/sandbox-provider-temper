package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// fakeSandboxes serves sandboxes from a map; "sb-0000000000db" simulates a
// database error. It counts lookups.
type fakeSandboxes struct {
	mu         sync.Mutex
	rows       map[string]store.Sandbox
	lookups    atomic.Int32
	touches    atomic.Int32
	touchDelay time.Duration
}

func (f *fakeSandboxes) TouchSandbox(ctx context.Context, _ string) error {
	if f.touchDelay > 0 { // a slow database
		select {
		case <-time.After(f.touchDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.touches.Add(1)
	return nil
}

func (f *fakeSandboxes) GetSandbox(_ context.Context, id string) (store.Sandbox, error) {
	f.lookups.Add(1)
	if id == "sb-0000000000db" {
		return store.Sandbox{}, errors.New("connection refused")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.rows[id]
	if !ok {
		return store.Sandbox{}, store.ErrNotFound
	}
	return sb, nil
}

func authorized(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " owner@laptop"
}

// tryLogin authenticates as user with key and reports whether the gateway
// accepted the login.
func tryLogin(t *testing.T, addr, user string, host ssh.PublicKey, key ssh.Signer) bool {
	t.Helper()
	c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.FixedHostKey(host),
	})
	if err != nil {
		if !strings.Contains(err.Error(), "unable to authenticate") {
			t.Fatalf("%s: unexpected error %v", user, err)
		}
		return false
	}
	c.Close()
	return true
}

func TestAuthentication(t *testing.T) {
	host := signer(t)
	owner, stranger, otherOwner := signer(t), signer(t), signer(t)
	f := &fakeSandboxes{rows: map[string]store.Sandbox{
		"sb-00000000000a": {ID: "sb-00000000000a", Status: store.StatusRunning, SSHPublicKey: authorized(owner.PublicKey())},
		"sb-00000000000b": {ID: "sb-00000000000b", Status: store.StatusRunning, SSHPublicKey: authorized(otherOwner.PublicKey())},
		"sb-00000000000c": {ID: "sb-00000000000c", Status: store.StatusPending, SSHPublicKey: authorized(owner.PublicKey())},
		"sb-00000000000d": {ID: "sb-00000000000d", Status: store.StatusDeleted, SSHPublicKey: authorized(owner.PublicKey())},
	}}
	addr := start(t, New(host, f, nil, quietLog()))

	for _, tc := range []struct {
		name string
		user string
		key  ssh.Signer
		want bool
	}{
		{"owner's key, running sandbox", "sb-00000000000a", owner, true},
		{"someone else's key", "sb-00000000000a", stranger, false},
		{"owner of another sandbox", "sb-00000000000a", otherOwner, false},
		{"right key, sandbox still pending", "sb-00000000000c", owner, false},
		{"right key, sandbox deleted", "sb-00000000000d", owner, false},
		{"no such sandbox", "sb-0000000000ff", owner, false},
		{"database error", "sb-0000000000db", owner, false},
	} {
		if got := tryLogin(t, addr, tc.user, host.PublicKey(), tc.key); got != tc.want {
			t.Errorf("%s: login accepted = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A user name that isn't a sandbox id is refused before any database work.
func TestBadUserNameSkipsLookup(t *testing.T) {
	host := signer(t)
	f := &fakeSandboxes{}
	addr := start(t, New(host, f, nil, quietLog()))
	for _, user := range []string{"root", "user", "sb-XYZ", "sb-0123456789ab; DROP TABLE"} {
		if tryLogin(t, addr, user, host.PublicKey(), signer(t)) {
			t.Fatalf("%q let in", user)
		}
	}
	if n := f.lookups.Load(); n != 0 {
		t.Fatalf("%d database lookups for invalid user names, want 0", n)
	}
}

// syncBuffer is a bytes.Buffer safe to write from the gateway's goroutines
// while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logBuffer captures log output so tests can check what was logged.
func logBuffer() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

type fakeMeta struct {
	ssh.ConnMetadata
	user string
}

func (m fakeMeta) User() string          { return m.user }
func (m fakeMeta) RemoteAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (m fakeMeta) SessionID() []byte     { return []byte("s") }
func (m fakeMeta) ClientVersion() []byte { return nil }

func running(id string, owner ssh.PublicKey) store.Sandbox {
	return store.Sandbox{ID: id, Status: store.StatusRunning, SSHPublicKey: authorized(owner)}
}

// A "query" (is this key OK?) runs the callback without proving the client
// holds the key. It must not be logged as a login; only a completed
// handshake is (review finding 1: someone with just the public key made
// the log say "login accepted").
func TestOnlyCompletedLoginsAreLoggedAsAccepted(t *testing.T) {
	host, owner := signer(t), signer(t)
	f := &fakeSandboxes{rows: map[string]store.Sandbox{"sb-00000000000a": running("sb-00000000000a", owner.PublicKey())}}
	log, buf := logBuffer()
	srv := New(host, f, nil, log)

	perms, err := (&connAuth{s: srv}).authenticate(fakeMeta{user: "sb-00000000000a"}, owner.PublicKey())
	if err != nil || perms.Extensions[permSandboxID] != "sb-00000000000a" {
		t.Fatalf("matching key: %v, %v", perms, err)
	}
	if strings.Contains(buf.String(), "login accepted") {
		t.Fatal("a key match without a completed handshake was logged as a login")
	}

	if !tryLogin(t, start(t, srv), "sb-00000000000a", host.PublicKey(), owner) {
		t.Fatal("owner login failed")
	}
	if n := strings.Count(buf.String(), "login accepted"); n != 1 {
		t.Fatalf("%d 'login accepted' lines after one real login, want 1:\n%s", n, buf.String())
	}
}

// One connection may only try one sandbox, and at most maxAuthTries lookups
// (review finding 2: up to 128 DB lookups per connection otherwise).
func TestPerConnectionLimits(t *testing.T) {
	owner := signer(t)
	f := &fakeSandboxes{rows: map[string]store.Sandbox{
		"sb-00000000000a": running("sb-00000000000a", owner.PublicKey()),
		"sb-00000000000b": running("sb-00000000000b", owner.PublicKey()),
	}}
	srv := New(signer(t), f, nil, quietLog())

	a := &connAuth{s: srv}
	if _, err := a.authenticate(fakeMeta{user: "sb-00000000000a"}, owner.PublicKey()); err != nil {
		t.Fatal(err)
	}
	before := f.lookups.Load()
	if _, err := a.authenticate(fakeMeta{user: "sb-00000000000b"}, owner.PublicKey()); err == nil {
		t.Fatal("user name changed mid-connection and was accepted")
	}
	if f.lookups.Load() != before {
		t.Fatal("a changed user name still cost a database lookup")
	}

	b := &connAuth{s: srv}
	stranger := signer(t)
	for i := 0; i < 50; i++ {
		b.authenticate(fakeMeta{user: "sb-00000000000a"}, stranger.PublicKey())
	}
	if got := f.lookups.Load() - before; got != maxAuthTries {
		t.Fatalf("%d lookups for 50 attempts on one connection, want %d", got, maxAuthTries)
	}
}

// While the database is down, one connection logs at most one warning
// (review finding 3).
func TestLookupErrorWarnsOncePerConnection(t *testing.T) {
	log, buf := logBuffer()
	a := &connAuth{s: New(signer(t), &fakeSandboxes{}, nil, log)}
	for i := 0; i < maxAuthTries; i++ {
		a.authenticate(fakeMeta{user: "sb-0000000000db"}, signer(t).PublicKey())
	}
	if n := strings.Count(buf.String(), "sandbox lookup failed"); n != 1 {
		t.Fatalf("%d lookup warnings for one connection, want 1", n)
	}
}

// Offering a wrong key first and the right one second still logs in (what
// OpenSSH does when several keys are loaded).
func TestWrongKeyThenRightKey(t *testing.T) {
	host, owner := signer(t), signer(t)
	f := &fakeSandboxes{rows: map[string]store.Sandbox{"sb-00000000000a": running("sb-00000000000a", owner.PublicKey())}}
	addr := start(t, New(host, f, nil, quietLog()))
	c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "sb-00000000000a",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer(t), signer(t), owner)},
		HostKeyCallback: ssh.FixedHostKey(host.PublicKey()),
	})
	if err != nil {
		t.Fatalf("owner key offered third was refused: %v", err)
	}
	c.Close()
}

func TestUnparseableStoredKeyIsRefused(t *testing.T) {
	f := &fakeSandboxes{rows: map[string]store.Sandbox{
		"sb-00000000000a": {ID: "sb-00000000000a", Status: store.StatusRunning, SSHPublicKey: "garbage"},
	}}
	a := &connAuth{s: New(signer(t), f, nil, quietLog())}
	if _, err := a.authenticate(fakeMeta{user: "sb-00000000000a"}, signer(t).PublicKey()); err == nil {
		t.Fatal("a sandbox with an unparseable stored key let someone in")
	}
}
