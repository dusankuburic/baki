package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// GenerateOpaqueToken mints a 256-bit random, URL-safe token for one-shot flows
// (password reset, email verification, invites). It returns the raw value (sent
// to the user, e.g. in an email link) and its hex SHA-256 hash (the only thing
// stored at rest, so a database read alone cannot redeem the token). SHA-256 is
// appropriate because the input is high-entropy and not brute-forceable.
func GenerateOpaqueToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashOpaqueToken(raw), nil
}

// HashOpaqueToken returns the hex SHA-256 of a raw opaque token.
func HashOpaqueToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
