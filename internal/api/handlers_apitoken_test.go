package api

import (
	"net/http"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
)

func TestAPITokenEndpoints_FullLifecycle(t *testing.T) {
	rt, _ := newLibraryTestRouter(t) // filesystem backend, JWT enabled
	// The PAT auth path resolves the owning user, so the user must exist.
	seedUserWithRole(t, rt, "alice", "alice@example.com", auth.RoleMember)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// Create — returns the raw token exactly once.
	create := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/tokens", bearer, map[string]any{"name": "ci"})
	checkStatus(t, create, http.StatusOK)
	var created struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	decodeJSON(t, create, &created)
	if created.ID == "" || created.Name != "ci" {
		t.Fatalf("unexpected create response: %+v", created)
	}
	if !auth.IsAPIToken(created.Token) {
		t.Fatalf("create did not return a usable raw token: %q", created.Token)
	}

	// The raw token authenticates a request on its own (no JWT).
	me := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+created.Token, nil)
	checkStatus(t, me, http.StatusOK)

	// List — returns metadata, and must never leak the hash or raw token.
	list := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/tokens", bearer, nil)
	checkStatus(t, list, http.StatusOK)
	if body := list.Body.String(); strings.Contains(body, "tokenHash") || strings.Contains(body, created.Token) {
		t.Errorf("token list leaked secret material: %s", body)
	}
	var tokens []map[string]any
	decodeJSON(t, list, &tokens)
	if len(tokens) != 1 || tokens[0]["id"] != created.ID {
		t.Fatalf("expected the one created token in the list, got %+v", tokens)
	}

	// Revoke — afterwards the raw token must no longer authenticate.
	del := doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/tokens/"+created.ID, bearer, nil)
	checkStatus(t, del, http.StatusOK)

	after := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+created.Token, nil)
	checkStatus(t, after, http.StatusUnauthorized)
}

func TestAPITokenEndpoints_RequireBackend(t *testing.T) {
	rt := newTestRouter(nil, false) // desktop/in-memory: no storage backend
	rr := doRequest(t, rt, http.MethodPost, "/api/auth/tokens", map[string]any{"name": "x"})
	checkStatus(t, rr, http.StatusServiceUnavailable)
}
