package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// mintToken seeds a user and an API token (by hash) directly via the backend and
// returns the raw token to present in the Authorization header. scopes nil/empty
// = unscoped (full access); otherwise the capability restriction under test.
func mintToken(t *testing.T, rt *Router, userID, email string, role auth.Role, expiresAt *time.Time, scopes ...string) string {
	t.Helper()
	seedUserWithRole(t, rt, userID, email, role)
	raw, hash, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if err := rt.security.Backend.CreateAPIToken(context.Background(), &storageif.APIToken{
		ID: "tok-" + userID, UserID: userID, Name: "ci", TokenHash: hash, ExpiresAt: expiresAt, Scopes: scopes,
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return raw
}

func TestAPIToken_AuthenticatesRequest(t *testing.T) {
	rt, _ := newLibraryTestRouter(t) // filesystem backend, JWT enabled
	raw := mintToken(t, rt, "alice", "alice@example.com", auth.RoleMember, nil)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+raw, nil)
	checkStatus(t, rr, http.StatusOK)
}

func TestAPIToken_UnknownRejected(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	// A well-formed but never-stored PAT must be rejected.
	raw := auth.APITokenPrefix + "deadbeefdeadbeefdeadbeefdeadbeef"

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+raw, nil)
	checkStatus(t, rr, http.StatusUnauthorized)
}

func TestAPIToken_ExpiredRejected(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	past := time.Now().Add(-1 * time.Hour)
	raw := mintToken(t, rt, "bob", "bob@example.com", auth.RoleMember, &past)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+raw, nil)
	checkStatus(t, rr, http.StatusUnauthorized)
}

func TestAPIToken_RevokedRejected(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	raw := mintToken(t, rt, "carol", "carol@example.com", auth.RoleMember, nil)

	// Sanity: works before revocation.
	if rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+raw, nil); rr.Code != http.StatusOK {
		t.Fatalf("expected 200 before revoke, got %d", rr.Code)
	}
	// Revoke (delete) the token; it must no longer authenticate.
	if err := rt.security.Backend.DeleteAPIToken(context.Background(), "carol", "tok-carol"); err != nil {
		t.Fatalf("delete token: %v", err)
	}
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+raw, nil)
	checkStatus(t, rr, http.StatusUnauthorized)
}

// TestAPIToken_ScopeEnforcement pins R2-1: a scoped PAT is capability-restricted
// at the auth middleware — before any handler runs. The motivating case: a CI
// token (read) must not mutate flows, reach chat (spends money), or manage
// credentials, while remaining a valid credential for reads.
func TestAPIToken_ScopeEnforcement(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	readOnly := mintToken(t, rt, "ci-user", "ci@example.com", auth.RoleMember, nil, auth.ScopeRead)

	// Reads pass.
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/analysis/rules", "Bearer "+readOnly, nil)
	checkStatus(t, rr, http.StatusOK)

	// Mutations are 403 (not 401 — the token IS valid, just not authorized).
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/apply-fix", "Bearer "+readOnly, map[string]any{"flowId": "x"})
	checkStatus(t, rr, http.StatusForbidden)

	// Chat is out of scope for a read token.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/stream", "Bearer "+readOnly, map[string]any{"flowId": "x"})
	checkStatus(t, rr, http.StatusForbidden)

	// Credential management is denied for EVERY scoped token.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/tokens", "Bearer "+readOnly, map[string]any{"name": "esc"})
	checkStatus(t, rr, http.StatusForbidden)
	// ...but the identity self-check works (CI validation use case).
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+readOnly, nil)
	checkStatus(t, rr, http.StatusOK)
}

// TestAPIToken_ScopeWriteTokenMutates: the write scope grants mutations.
func TestAPIToken_ScopeWriteTokenMutates(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	writer := mintToken(t, rt, "ci-writer", "w@example.com", auth.RoleMember, nil, auth.ScopeWrite)

	// A mutation attempt now passes the scope gate (the 400/404 that follows
	// is the handler's normal validation, proving the request reached it).
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/apply-fix", "Bearer "+writer, map[string]any{"flowId": "nope"})
	if rr.Code == http.StatusForbidden {
		t.Fatalf("write-scoped token rejected at the scope gate: %s", rr.Body.String())
	}
}

// TestAPIToken_UnscopedBackwardCompatible: tokens with no scope list (every
// pre-existing token) keep full access — no behavioral change on upgrade.
func TestAPIToken_UnscopedBackwardCompatible(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	full := mintToken(t, rt, "legacy", "legacy@example.com", auth.RoleMember, nil) // no scopes

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/analysis/rules", "Bearer "+full, nil)
	checkStatus(t, rr, http.StatusOK)
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/apply-fix", "Bearer "+full, map[string]any{"flowId": "x"})
	if rr.Code == http.StatusForbidden {
		t.Fatalf("unscoped legacy token hit the scope gate: %s", rr.Body.String())
	}
}
