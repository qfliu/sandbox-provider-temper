package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// public-key reads the PKCS#8 PEM keys the Helm chart generates and prints
// the matching OpenSSH public key.
func TestPublicKeyFromPKCS8(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runPublicKey([]string{file}, &out); err != nil {
		t.Fatal(err)
	}
	want, _ := ssh.NewPublicKey(pub)
	if out.String() != string(ssh.MarshalAuthorizedKey(want)) {
		t.Fatalf("got %q, want %q", out.String(), ssh.MarshalAuthorizedKey(want))
	}
	if err := runPublicKey(nil, &out); err == nil {
		t.Fatal("no argument should fail")
	}
}
