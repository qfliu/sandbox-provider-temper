package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// startServer runs a tiny in-process SSH server with the given host key that
// accepts only clientKey, answering every "exec" with "ok".
func startServer(t *testing.T, hostKeyPEM []byte, clientKey ssh.PublicKey) string {
	t.Helper()
	hostSigner, err := ssh.ParsePrivateKey(hostKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(clientKey.Marshal()) {
				return nil, nil
			}
			return nil, os.ErrPermission
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
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
					ch, creqs, _ := nch.Accept()
					go func() {
						for r := range creqs {
							r.Reply(r.Type == "exec", nil)
							if r.Type == "exec" {
								ch.Write([]byte("ok"))
								ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
								ch.Close()
							}
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func newClientSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDialAcceptsOnlyTheRecordedHostKey(t *testing.T) {
	client := newClientSigner(t)
	hostPEM, hostPub, err := GenerateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	addr := startServer(t, hostPEM, client.PublicKey())
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
	addr := startServer(t, hostPEM, newClientSigner(t).PublicKey())

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
