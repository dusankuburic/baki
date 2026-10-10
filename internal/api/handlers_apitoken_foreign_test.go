package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/testutil"
)

// Deleting another user's token id is a no-op: the store refuses the delete, and so must the revocation that follows
// it. The token's live sockets are cut by blacklisting its derived JTI, so revoking whatever id a caller names let
// anyone who learned another user's token id (an audit row shows it) close that token's sockets and refuse it new ones.
func TestAPITokenEndpoints_AnotherUsersTokenIsNotRevoked(t *testing.T) {
	store := testutil.NewFakeBackend()
	rt := newTestRouter(store, true)
	seedUserWithRole(t, rt, "alice", "alice@example.com", auth.RoleMember)
	seedUserWithRole(t, rt, "mallory", "mallory@example.com", auth.RoleMember)
	alice := jwtBearer(t, rt, "alice", "alice@example.com")
	mallory := jwtBearer(t, rt, "mallory", "mallory@example.com")

	create := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/tokens", alice, map[string]any{"name": "ci"})
	checkStatus(t, create, http.StatusOK)
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decodeJSON(t, create, &created)
	revokes := func() int {
		n := 0
		for _, ev := range store.SavedAuditEvents() {
			if ev.Action == "token.revoke" && ev.ResourceID == created.ID {
				n++
			}
		}
		return n
	}
	before := revokes()

	// The answer stays the idempotent 200 an unknown id gets: it says nothing about whose id it is.
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/tokens/"+created.ID, mallory, nil), http.StatusOK)
	if rt.security.AuthMgr.IsRevoked(auth.PATJTI(created.ID)) {
		t.Error("another user's delete blacklisted the token: its sockets would be closed")
	}
	if n := revokes(); n != before {
		t.Errorf("another user's delete wrote %d token.revoke audit rows for a token it did not revoke", n-before)
	}
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+created.Token, nil), http.StatusOK)

	// The owner's delete still revokes.
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/tokens/"+created.ID, alice, nil), http.StatusOK)
	if !rt.security.AuthMgr.IsRevoked(auth.PATJTI(created.ID)) {
		t.Error("the owner's delete did not blacklist the token")
	}
}
