package auth

import (
	"net/http"
	"testing"
)

// TestRequiredScope pins the (method, path) → scope mapping the router
// enforces for scoped PATs: reads → read, mutations → write, chat/admin
// prefixes their own, credential management denied.
func TestRequiredScope(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{http.MethodGet, "/api/analysis/rules", ScopeRead},
		{http.MethodHead, "/api/flow/source", ScopeRead},
		{http.MethodPost, "/api/flow/apply-fix", ScopeWrite},
		{http.MethodDelete, "/api/library/x", ScopeWrite},
		{http.MethodPut, "/api/library/x", ScopeWrite},
		{http.MethodPost, "/api/chat/stream", ScopeChat},
		{http.MethodGet, "/api/chat/get", ScopeChat},
		{http.MethodGet, "/api/admin/system/health", ScopeAdmin},
		{http.MethodPost, "/api/admin/scanner/scan", ScopeAdmin},
		{http.MethodGet, "/api/auth/me", ScopeRead},
		{http.MethodGet, "/api/auth/tokens", ScopeDeny},
		{http.MethodPost, "/api/auth/tokens", ScopeDeny},
		{http.MethodPost, "/api/auth/password", ScopeDeny},
		{http.MethodGet, "/api/ws-ticket", ScopeRead},
	}
	for _, tc := range cases {
		if got := RequiredScope(tc.method, tc.path); got != tc.want {
			t.Errorf("RequiredScope(%s %s) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

// TestRouteAllowed pins the capability check: membership semantics, deny
// sentinel, and that the empty scope list allows nothing (scoped means
// restricted).
func TestRouteAllowed(t *testing.T) {
	read := []string{ScopeRead}
	readWrite := []string{ScopeRead, ScopeWrite}

	if !RouteAllowed(http.MethodGet, "/api/analysis/rules", read) {
		t.Error("read token must GET")
	}
	if RouteAllowed(http.MethodPost, "/api/flow/apply-fix", read) {
		t.Error("read token must not mutate")
	}
	if !RouteAllowed(http.MethodPost, "/api/flow/apply-fix", readWrite) {
		t.Error("write token must mutate")
	}
	if RouteAllowed(http.MethodPost, "/api/chat/stream", readWrite) {
		t.Error("chat endpoints need the chat scope (they spend money)")
	}
	if !RouteAllowed(http.MethodPost, "/api/chat/stream", []string{ScopeChat}) {
		t.Error("chat token must reach chat")
	}
	if RouteAllowed(http.MethodGet, "/api/auth/tokens", []string{ScopeRead, ScopeWrite, ScopeChat, ScopeAdmin}) {
		t.Error("no scope grants credential management")
	}
	if RouteAllowed(http.MethodGet, "/api/analysis/rules", nil) {
		t.Error("scoped-with-empty-list must allow nothing (restriction is the point)")
	}
}

// TestValidScope: closed set membership.
func TestValidScope(t *testing.T) {
	for _, ok := range ValidTokenScopes {
		if !ValidScope(ok) {
			t.Errorf("ValidScope(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "READ", "root", "read ", "flows"} {
		if ValidScope(bad) {
			t.Errorf("ValidScope(%q) = true, want false", bad)
		}
	}
}
