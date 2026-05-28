package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/auth"
)

// TestRequireRole_CloudMode verifies that admin-gated endpoints enforce the
// admin role server-side in cloud (JWT) mode — a non-admin token must be
// rejected with 403 and a missing token with 401, regardless of what the
// frontend shows.
func TestRequireRole_CloudMode(t *testing.T) {
	rt := &Router{jwtEnabled: true}

	cases := []struct {
		name     string
		role     auth.Role
		want     bool
		wantCode int
	}{
		{"admin allowed", auth.RoleAdmin, true, http.StatusOK},
		{"member forbidden", auth.RoleMember, false, http.StatusForbidden},
		{"viewer forbidden", auth.RoleViewer, false, http.StatusForbidden},
		{"guest forbidden", auth.RoleGuest, false, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/admin/users/list", nil)
			req = req.WithContext(auth.WithClaims(req.Context(), &auth.Claims{Role: tc.role}))
			rec := httptest.NewRecorder()

			got := rt.requireRole(rec, req, auth.RoleAdmin)
			if got != tc.want {
				t.Fatalf("requireRole(role=%s) = %v, want %v", tc.role, got, tc.want)
			}
			if !tc.want && rec.Code != tc.wantCode {
				t.Fatalf("requireRole(role=%s) status = %d, want %d", tc.role, rec.Code, tc.wantCode)
			}
		})
	}
}

// TestRequireRole_MissingClaims verifies that a request with no claims in
// context (no/invalid token) is rejected with 401 in cloud mode.
func TestRequireRole_MissingClaims(t *testing.T) {
	rt := &Router{jwtEnabled: true}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/list", nil)
	rec := httptest.NewRecorder()

	if rt.requireRole(rec, req, auth.RoleAdmin) {
		t.Fatal("requireRole with nil claims should be denied")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("requireRole with nil claims status = %d, want 401", rec.Code)
	}
}

// TestRequireRole_LocalModeBypass documents the intentional behaviour that in
// local (single-user, token-gated) mode role checks are skipped — the desktop
// user is implicitly the administrator.
func TestRequireRole_LocalModeBypass(t *testing.T) {
	rt := &Router{jwtEnabled: false}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/list", nil)
	rec := httptest.NewRecorder()

	if !rt.requireRole(rec, req, auth.RoleAdmin) {
		t.Fatal("local mode should allow admin actions without JWT claims")
	}
}
