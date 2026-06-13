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

		// Static API paths (no dynamic segment)
		{"/api/flows", "/api/flows"},
		{"/api/auth/login", "/api/auth/login"},

		// Root
		{"/", "/"},
		{"", "/"},

		// Static assets
		{"/assets/main.js", "/static/*"},
		{"/favicon.ico", "/static/*"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := normalizeRoute(tt.path); got != tt.expected {
				t.Errorf("normalizeRoute(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}
