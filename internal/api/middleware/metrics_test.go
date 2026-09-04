package middleware

import (
	"testing"
)

func TestNormalizeRoute(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		// Library: dynamic ID collapsed
		{"/api/library/abc123", "/api/library/:id"},
		{"/api/library/abc123/export", "/api/library/:id"},

		// Flow collaborators
		{"/api/flows/123/collaborators", "/api/flows/:id/collaborators"},
		{"/api/flows/123/collaborators/456", "/api/flows/:id/collaborators"},

		// Admin user role
		{"/api/admin/users/abc/role", "/api/admin/users/:id/role"},

		// Orgs
		{"/api/orgs/org-1", "/api/orgs/:id"},
		{"/api/orgs/org-1/members", "/api/orgs/:id/members"},
		{"/api/orgs/org-1/members/user-1", "/api/orgs/:id/members/:userId"},
		{"/api/orgs/org-1/members/user-1/role", "/api/orgs/:id/members/:userId"},

		// Swagger
		{"/swagger/index.html", "/swagger/*"},
		{"/swagger/", "/swagger/*"},

		// Health & metrics (exact matches)
		{"/healthz", "/healthz"},
		{"/readyz", "/readyz"},
		{"/api/health", "/api/health"},
		{"/metrics", "/metrics"},

		// Registered static API paths pass through verbatim.
		{"/api/auth/login", "/api/auth/login"},
		{"/api/flow/upload", "/api/flow/upload"},

		// Dynamic families that used to fall through to the verbatim /api/
		// case, minting one time series per identifier.
		{"/api/auth/sessions/sess-abc", "/api/auth/sessions/:id"},
		{"/api/auth/tokens/tok-abc", "/api/auth/tokens/:id"},
		{"/api/system/settings/org/org-9", "/api/system/settings/org/:id"},
		// The invite segment is the emailed single-use credential; it must
		// never reach a scraped, retained label.
		{"/api/invites/s3cret-invite-token/accept", "/api/invites/:token/accept"},

		// Root
		{"/", "/"},
		{"", "/"},

		// Static assets
		{"/assets/main.js", "/static/*"},
		{"/favicon.ico", "/static/*"},
	}

	known := map[string]struct{}{
		"/api/auth/login":  {},
		"/api/flow/upload": {},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := normalizeRoute(tt.path, known); got != tt.expected {
				t.Errorf("normalizeRoute(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}

// TestNormalizeRoute_UnknownAPIPathsAreBounded is the regression test for the
// metrics cardinality bomb. The Metrics middleware runs OUTSIDE the mux and
// BEFORE auth, so every request — unauthenticated, unrouted, 404 — reaches it.
// The old `/api/` fallthrough returned the path verbatim, so an attacker
// minted one Prometheus time series per request until the process ran out of
// memory. Unregistered paths must now collapse to a single bounded label.
func TestNormalizeRoute_UnknownAPIPathsAreBounded(t *testing.T) {
	known := map[string]struct{}{"/api/auth/login": {}}

	seen := map[string]struct{}{}
	for _, p := range []string{
		"/api/aaaaaaaa", "/api/bbbbbbbb", "/api/x/y/z",
		"/api/00000000-0000-0000-0000-000000000000",
		"/api/../api/nope", // already path.Clean'd by the router in practice
	} {
		got := normalizeRoute(p, known)
		if got != routeOther {
			t.Errorf("normalizeRoute(%q) = %q, want %q", p, got, routeOther)
		}
		seen[got] = struct{}{}
	}
	if len(seen) != 1 {
		t.Errorf("unknown paths produced %d distinct labels, want 1", len(seen))
	}

	// A nil set is fail-closed: unrecognized rather than unbounded.
	if got := normalizeRoute("/api/auth/login", nil); got != routeOther {
		t.Errorf("nil known set: got %q, want %q (fail-closed)", got, routeOther)
	}
}
