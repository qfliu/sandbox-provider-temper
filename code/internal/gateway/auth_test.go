package gateway

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// fakeSandboxes serves sandboxes from a map; "sb-0000000000db" simulates a
// database error. It counts lookups.
type fakeSandboxes struct {
	rows    map[string]store.Sandbox
	lookups atomic.Int32
}

func (f *fakeSandboxes) GetSandbox(_ context.Context, id string) (store.Sandbox, error) {
	f.lookups.Add(1)
	if id == "sb-0000000000db" {
		return store.Sandbox{}, errors.New("connection refused")
	}
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
// accepted the login. A successful login can't open a session yet (step
// 3.3), which is checked too.
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
	defer c.Close()
	if _, err := c.NewSession(); err == nil || !strings.Contains(err.Error(), "not proxied yet") {
		t.Fatalf("%s: session = %v; sessions aren't proxied until step 3.3", user, err)
	}
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
	addr := start(t, New(host, f, quietLog()))

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
	addr := start(t, New(host, f, quietLog()))
	for _, user := range []string{"root", "user", "sb-XYZ", "sb-0123456789ab; DROP TABLE"} {
		if tryLogin(t, addr, user, host.PublicKey(), signer(t)) {
			t.Fatalf("%q let in", user)
		}
	}
	if n := f.lookups.Load(); n != 0 {
		t.Fatalf("%d database lookups for invalid user names, want 0", n)
	}
}
