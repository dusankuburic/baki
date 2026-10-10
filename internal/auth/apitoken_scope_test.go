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
		{http.MethodGet, "/api/orgs/o1/ci-credentials", ScopeRead},
		{http.MethodPost, "/api/orgs/o1/ci-credentials", ScopeDeny},
		{http.MethodDelete, "/api/orgs/o1/ci-credentials/k1", ScopeDeny},
		{http.MethodPost, "/api/orgs/o1/ci-credentials-x", ScopeWrite},
		{http.MethodPost, "/api/orgs//ci-credentials", ScopeWrite},
		{http.MethodPost, "/api/integrations/ci", ScopeCI},
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
	if RouteAllowed(http.MethodPost, "/api/chat/fix-decision", []string{ScopeChat}) {
		t.Error("approving a fix mutates the flow: chat alone must not be enough")
	}
	if RouteAllowed(http.MethodPost, "/api/chat/fix-decision", readWrite) {
		t.Error("fix-decision is still a chat endpoint: write alone must not be enough")
	}
	if !RouteAllowed(http.MethodPost, "/api/chat/fix-decision", []string{ScopeChat, ScopeWrite}) {
		t.Error("chat+write token must be able to approve a fix")
	}
	if RouteAllowed(http.MethodGet, "/api/auth/tokens", ValidTokenScopes) {
		t.Error("no scope grants credential management")
	}
	if RouteAllowed(http.MethodGet, "/api/analysis/rules", nil) {
		t.Error("scoped-with-empty-list must allow nothing (restriction is the point)")
	}
	ci := []string{ScopeCI}
	if RouteAllowed(http.MethodGet, "/api/analysis/rules", ci) || RouteAllowed(http.MethodPost, "/api/analysis/analyze", ci) || RouteAllowed(http.MethodGet, "/api/auth/me", ci) {
		t.Error("the ci scope must grant nothing outside the CI webhook")
	}
	if !RouteAllowed(http.MethodPost, "/api/integrations/ci", ci) || RouteAllowed(http.MethodPost, "/api/integrations/ci", readWrite) {
		t.Error("the CI webhook takes the ci scope, and read+write is not it")
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
