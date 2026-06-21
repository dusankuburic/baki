package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// APITokenPrefix marks a value as a machine token (personal access token) rather
// than a JWT, so the auth middleware can route it to hash-based verification. It
// is intentionally human-recognizable in logs and config.
const APITokenPrefix = "pad_pat_"

// GenerateAPIToken mints a new machine token: a 256-bit random secret with the
// PAT prefix. It returns the raw token (shown to the user once) and its hash
// (stored at rest — the raw value is unrecoverable from the hash).
func GenerateAPIToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	raw = APITokenPrefix + hex.EncodeToString(b)
	return raw, HashAPIToken(raw), nil
}

// HashAPIToken returns the hex SHA-256 of a raw token. SHA-256 (not bcrypt) is
// appropriate here: the input is a 256-bit uniformly-random secret, not a
// low-entropy password, so it is not brute-forceable and a fast hash keeps the
// per-request auth lookup cheap.
func HashAPIToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// IsAPIToken reports whether a bearer value is a machine token.
func IsAPIToken(token string) bool {
	return strings.HasPrefix(token, APITokenPrefix)
}
