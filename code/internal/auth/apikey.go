// Package auth generates and hashes API keys.
//
// A key looks like "sk_" + 43 URL-safe characters (32 random bytes). Only its
// SHA-256 hash is stored. The plain key is shown once, when it is created.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

const (
	keyPrefix = "sk_"
	// DisplayPrefixLen is how much of a key is kept in plain text (e.g.
	// "sk_3f9aB1c2") so users can tell their keys apart in the dashboard.
	// 8 random characters reveal too little to help guess the rest.
	DisplayPrefixLen = len(keyPrefix) + 8
)

// NewKey returns a new random API key, its display prefix and its hash.
func NewKey() (plaintext, displayPrefix string, hash []byte) {
	buf := make([]byte, 32)
	rand.Read(buf) // crypto/rand.Read never fails on supported platforms
	plaintext = keyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, plaintext[:DisplayPrefixLen], Hash(plaintext)
}

// Hash returns the value stored in api_keys.key_hash for a plain key.
//
// SHA-256 (fast) is fine here because keys are 256 bits of randomness, so
// brute-forcing a leaked hash is infeasible. Slow hashes like bcrypt only
// matter for low-entropy secrets like human passwords.
func Hash(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// LooksLikeKey is a cheap format check, so garbage tokens are rejected
// without a database lookup.
func LooksLikeKey(s string) bool {
	return strings.HasPrefix(s, keyPrefix) && len(s) == len(keyPrefix)+43
}
