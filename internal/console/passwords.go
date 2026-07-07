// Package console is the admin console's authentication layer: signed session
// cookies, local (username/password) accounts, SSO identity → admin authorization,
// and the CSRF/state signing the OAuth flows use. It is stdlib-only (no external
// crypto dependency): PBKDF2 (crypto/pbkdf2) for passwords, HMAC-SHA256 for the
// signed cookies/state. It holds no vendor logic and imports no other internal
// package.
package console

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"
)

// pbkdf2Iter is the work factor for local-account passwords. High enough to be
// costly to brute-force, cheap enough for an interactive login.
const pbkdf2Iter = 210_000

// Verification FLOORS (defense-in-depth): a stored hash below these is rejected so
// a weak/downgraded config hash (e.g. iter=1, 1-byte digest) can't silently accept
// passwords cheaply. Floors, not equalities — a larger/rotated parameter still ok.
const (
	minPBKDF2Iter = 100_000
	minDKLen      = 16
)

// HashPassword returns a self-describing PBKDF2-HMAC-SHA256 hash string
// ("pbkdf2$sha256$<iter>$<salt_b64>$<dk_b64>") with a fresh 16-byte salt. Operators
// store the RESULT in config; the plaintext is never persisted.
func HashPassword(plaintext string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, plaintext, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return "pbkdf2$sha256$" + strconv.Itoa(pbkdf2Iter) + "$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(dk), nil
}

// VerifyPassword reports whether plaintext matches an encoded PBKDF2 hash. The
// final compare is constant-time; a malformed encoding returns false (never
// panics), so a corrupt config row denies rather than crashes the login path.
func VerifyPassword(encoded, plaintext string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter < minPBKDF2Iter {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) < minDKLen {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(want) < minDKLen {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, plaintext, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}
