package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewKey(t *testing.T) {
	key, prefix, hash := NewKey()

	if !LooksLikeKey(key) {
		t.Fatalf("generated key %q fails its own format check", key)
	}
	if !strings.HasPrefix(key, prefix) || len(prefix) != DisplayPrefixLen {
		t.Fatalf("prefix %q is not the first %d chars of %q", prefix, DisplayPrefixLen, key)
	}
	if !bytes.Equal(hash, Hash(key)) {
		t.Fatal("returned hash doesn't match Hash(key)")
	}
	if bytes.Contains(hash, []byte(key)) {
		t.Fatal("hash contains the plain key")
	}
}

func TestNewKeyIsRandom(t *testing.T) {
	a, _, _ := NewKey()
	b, _, _ := NewKey()
	if a == b {
		t.Fatal("two generated keys are identical")
	}
}

func TestLooksLikeKey(t *testing.T) {
	for _, s := range []string{"", "sk_", "sk_short", "pk_" + strings.Repeat("a", 43), "sk_" + strings.Repeat("a", 44)} {
		if LooksLikeKey(s) {
			t.Errorf("LooksLikeKey(%q) = true, want false", s)
		}
	}
}
