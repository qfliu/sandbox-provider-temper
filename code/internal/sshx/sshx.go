// Package sshx holds the platform's SSH plumbing: generating per-sandbox
// host keys, loading the internal client key, and connecting to sandboxes
// with host-key pinning.
package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SandboxUser is the account the platform logs in as inside a sandbox.
const SandboxUser = "user"

// GenerateHostKey returns a new ed25519 host key for one sandbox: the
// private key in OpenSSH PEM format (for sshd's HostKey file) and the public
// key in authorized_keys format (stored in the database).
func GenerateHostKey() (privatePEM []byte, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "sandbox host key")
	if err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(block), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), nil
}

// LoadSigner reads the platform-internal private key (D7) from a file.
func LoadSigner(path string) (ssh.Signer, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read internal SSH key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parse internal SSH key %s: %w", path, err)
	}
	return signer, nil
}

// Dialer opens SSH connections to sandboxes as SandboxUser.
type Dialer struct {
	Signer  ssh.Signer
	Timeout time.Duration // TCP connect + SSH handshake; default 10s
}

// Dial connects to a sandbox's sshd at addr (ip:port) and succeeds only if
// the server proves it holds the private half of hostPublicKey. Without this
// check, a reused pod IP could lead into a different user's sandbox, since
// every sandbox trusts the same internal key (D29).
func (d Dialer) Dial(ctx context.Context, addr, hostPublicKey string) (*ssh.Client, error) {
	if hostPublicKey == "" {
		return nil, fmt.Errorf("no host key recorded for %s; refusing to connect", addr)
	}
	want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostPublicKey))
	if err != nil {
		return nil, fmt.Errorf("parse recorded host key: %w", err)
	}
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cfg := &ssh.ClientConfig{
		User:              SandboxUser,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(d.Signer)},
		HostKeyCallback:   ssh.FixedHostKey(want),
		HostKeyAlgorithms: []string{want.Type()},
		Timeout:           timeout,
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// Bound the handshake too; NewClientConn has no context of its own.
	conn.SetDeadline(time.Now().Add(timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}
