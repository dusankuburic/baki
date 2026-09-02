package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
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

// ── Token scopes (R2-1) ─────────────────────────────────────────────────────
//
// A PAT minted WITHOUT scopes is "unscoped" — full access as the issuing user
// (backward compatible with every existing token). A PAT minted WITH scopes is
// capability-restricted: RouteAllowed maps (method, path) → the one scope the
// request needs, and any route that would need a capability the token lacks is
// rejected with 403 at the auth middleware, before any handler runs.
//
// The point: a CI token should never be able to delete flows, read chat
// history, or change admin settings — today every PAT is the full user.

// Token scopes. The set is intentionally small; each is a broad capability,
// not a per-route permission.
const (
	// ScopeRead covers all read-only routes (analysis reports, flows,
	// library, dashboards).
	ScopeRead = "read"
	// ScopeWrite covers mutations (saving flows, applying fixes, triage).
	ScopeWrite = "write"
	// ScopeChat covers the AI chat endpoints — they spend money, so a
	// leakage-resistant default is to leave them out of CI tokens.
	ScopeChat = "chat"
	// ScopeAdmin covers /api/admin/* (connector control, scanner triggers,
	// system health).
	ScopeAdmin = "admin"
)

// ValidTokenScopes is the closed set accepted at mint time.
var ValidTokenScopes = []string{ScopeRead, ScopeWrite, ScopeChat, ScopeAdmin}

// ValidScope reports whether s is a member of the closed scope set.
func ValidScope(s string) bool {
	switch s {
	case ScopeRead, ScopeWrite, ScopeChat, ScopeAdmin:
		return true
	}
	return false
}

// RequiredScope maps a request to the scope a SCOPED token would need for it.
// "" means no scope requirement (unscoped behavior). Read routes need
// ScopeRead; mutations need ScopeWrite; chat and admin prefixes have their own
// scopes. Token management (/api/auth/*) is denied to scoped tokens entirely —
// a read-scoped token minting itself full-access tokens would be privilege
// escalation, so there is no scope that grants it.
func RequiredScope(method, path string) string {
	switch {
	case strings.HasPrefix(path, "/api/admin/"):
		return ScopeAdmin
	case strings.HasPrefix(path, "/api/chat/"):
		return ScopeChat
	case path == "/api/auth/me":
		// Identity self-check only — useful for CI to validate its token,
		// and it reveals nothing beyond the caller's own identity.
		return ScopeRead
	case strings.HasPrefix(path, "/api/auth/"):
		return ScopeDeny // scoped tokens never manage credentials/identity
	case strings.HasPrefix(path, "/api/ws-ticket"):
		// The ticket is the entry to live updates (SSE/WS); read-level.
		return ScopeRead
	case method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions:
		return ScopeRead
	default:
		return ScopeWrite
	}
}

// ScopeDeny is RequiredScope's sentinel for routes scoped tokens may never
// touch regardless of their scope list.
const ScopeDeny = "\x00deny"

// RouteAllowed reports whether a scoped token may perform this request.
func RouteAllowed(method, path string, scopes []string) bool {
	required := RequiredScope(method, path)
	if required == "" || required == ScopeDeny {
		return required == ""
	}
	for _, s := range scopes {
		if s == required {
			return true
		}
	}
	return false
}
