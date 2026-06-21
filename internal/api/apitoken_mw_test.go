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
// returns the raw token to present in the Authorization header.
func mintToken(t *testing.T, rt *Router, userID, email string, role auth.Role, expiresAt *time.Time) string {
	t.Helper()
	seedUserWithRole(t, rt, userID, email, role)
	raw, hash, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if err := rt.security.Backend.CreateAPIToken(context.Background(), &storageif.APIToken{
		ID: "tok-" + userID, UserID: userID, Name: "ci", TokenHash: hash, ExpiresAt: expiresAt,
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
