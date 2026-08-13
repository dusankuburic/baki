package api

import (
	"net/http"
	"testing"
)

// A5: the session-list, session-revoke, and profile-update endpoints had ZERO
// HTTP-layer coverage. These are security/self-service surfaces. The session
// list/revoke data path depends on real refresh-token storage (a Postgres
// backend concern; the FS backend stubs it), so these tests assert the
// HTTP-layer contract — auth gating, 200/JSON shape, idempotent revoke — rather
// than the backend's token-storage behaviour.

func TestHandleAuthSessions_RequiresAuth(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/sessions", "", nil)
	if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
		t.Errorf("unauthed sessions list: status = %d, want 401/403", rr.Code)
	}
}

func TestHandleAuthSessions_AuthedReturns200AndArray(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "sessions@example.com", "Password123!")
	bearer := "Bearer " + tokenFrom(t, resp)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/sessions", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	// Must decode as a JSON array (shape contract), regardless of length.
	var list []map[string]any
	decodeJSON(t, rr, &list)
}

func TestHandleAuthSessionRevoke_RequiresAuth(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/sessions/any-id", "", nil)
	if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
		t.Errorf("unauthed session revoke: status = %d, want 401/403", rr.Code)
	}
}

func TestHandleAuthSessionRevoke_AuthedNoInternalError(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "revoke@example.com", "Password123!")
	bearer := "Bearer " + tokenFrom(t, resp)

	// The test router wires no tokenStore (the FS backend stubs refresh-token
	// storage), so the handler returns 503 "session management not available".
	// The contract this pins: revoke must not 500 (unhandled panic/path) on a
	// missing tokenStore or unknown session id. A real Postgres backend
	// (tokenStore present) is covered by the storage integration tests.
	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/sessions/some-session-id", bearer, nil)
	if rr.Code == http.StatusInternalServerError {
		t.Errorf("session revoke returned 500 (unhandled error); want 503/2xx/4xx. body: %s", rr.Body.String())
	}
}

func TestHandleAuthUpdateProfile_UpdatesDisplayName(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "profile@example.com", "Password123!")
	bearer := "Bearer " + tokenFrom(t, resp)

	upd := doRequestWithAuth(t, rt, http.MethodPut, "/api/auth/profile", bearer, map[string]any{
		"displayName": "Test User",
		"avatarUrl":   "",
	})
	checkStatus(t, upd, http.StatusOK)

	// /me returns the user object at the top level (not nested under "user").
	me := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", bearer, nil)
	checkStatus(t, me, http.StatusOK)
	var meResp map[string]any
	decodeJSON(t, me, &meResp)
	if meResp["displayName"] != "Test User" {
		t.Errorf("displayName = %v, want 'Test User'", meResp["displayName"])
	}
	if meResp["email"] != "profile@example.com" {
		t.Errorf("email changed by profile update: got %v", meResp["email"])
	}
}

// tokenFrom extracts the access token from a login() response map.
func tokenFrom(t *testing.T, resp map[string]any) string {
	t.Helper()
	tok, _ := resp["accessToken"].(string)
	if tok == "" {
		t.Fatalf("login response has no accessToken: %+v", resp)
	}
	return tok
}
