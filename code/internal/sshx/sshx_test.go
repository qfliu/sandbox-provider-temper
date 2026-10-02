package sshx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDialAcceptsOnlyTheRecordedHostKey(t *testing.T) {
	client := newClientSigner(t)
	hostPEM, hostPub, err := GenerateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	addr := startServer(t, hostPEM, client.PublicKey(), nil).addr
	d := Dialer{Signer: client}

	c, err := d.Dial(context.Background(), addr, hostPub)
	if err != nil {
		t.Fatalf("correct host key rejected: %v", err)
	}
	sess, _ := c.NewSession()
	out, err := sess.Output("anything")
	if err != nil || string(out) != "ok" {
		t.Fatalf("exec over pinned connection = %q, %v", out, err)
	}
	c.Close()

	// Same address, but we expected a different sandbox's key: must refuse.
	_, otherPub, _ := GenerateHostKey()
	if _, err := d.Dial(context.Background(), addr, otherPub); err == nil ||
		!strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("wrong host key: err = %v, want host key mismatch", err)
	}
	if _, err := d.Dial(context.Background(), addr, ""); err == nil {
		t.Fatal("empty recorded host key must refuse to connect")
	}
}

func TestDialRejectedWithoutTheInternalKey(t *testing.T) {
	hostPEM, hostPub, _ := GenerateHostKey()
	addr := startServer(t, hostPEM, newClientSigner(t).PublicKey(), nil).addr

	if _, err := (Dialer{Signer: newClientSigner(t)}).Dial(context.Background(), addr, hostPub); err == nil {
		t.Fatal("server accepted a client key it doesn't trust")
	}
}

func TestLoadSigner(t *testing.T) {
	pemBytes, _, _ := GenerateHostKey() // any OpenSSH private key will do
	path := filepath.Join(t.TempDir(), "id")
	os.WriteFile(path, pemBytes, 0o600)

	if _, err := LoadSigner(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file should be an error")
	}
}
