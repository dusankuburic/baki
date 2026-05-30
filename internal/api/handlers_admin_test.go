package api

import (
	"context"
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// seedUserWithRole inserts a user with the given role directly via the backend.
func seedUserWithRole(t *testing.T, rt *Router, id, email string, role auth.Role) {
	t.Helper()
	u := &storageif.User{
		ID:       id,
		Email:    email,
		Password: "hash",
		Role:     role,
	}
	if err := rt.app.StorageBackend().SaveUser(context.Background(), u); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// adminBearer returns an admin bearer token for the given seeded user.
func adminBearer(t *testing.T, rt *Router, userID, email string) string {
	t.Helper()
	pair, err := rt.authMgr.Issue(userID, email, auth.RoleAdmin)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	return "Bearer " + pair.AccessToken
}

// TestAdminUserRole_DemotingLastAdmin_Returns409 verifies the N-5 safeguard:
// when an admin tries to demote themselves (or any admin) while they are the
// only admin in the system, the request is refused with 409 Conflict and the
// role is left unchanged. Without this, a one-admin instance could be locked
// out of admin functions entirely.
func TestAdminUserRole_DemotingLastAdmin_Returns409(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := adminBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost,
		"/api/admin/users/admin-1/role", bearer,
		map[string]any{"role": string(auth.RoleMember)},
	)
	if rr.Code != http.StatusConflict {
		t.Errorf("expected 409 when demoting last admin, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	// Verify the role was not changed.
	u, err := rt.app.StorageBackend().LoadUserByID(context.Background(), "admin-1")
	if err != nil {
		t.Fatalf("LoadUserByID: %v", err)
	}
	if u.Role != auth.RoleAdmin {
		t.Errorf("role unexpectedly changed to %q after refused demotion", u.Role)
	}
}

// TestAdminUserRole_DemotingAdminWithOtherAdmins_Succeeds verifies the
// safeguard fires *only* when this is the last admin. Two admins → demoting
// one is fine.
func TestAdminUserRole_DemotingAdminWithOtherAdmins_Succeeds(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin1@example.com", auth.RoleAdmin)
	seedUserWithRole(t, rt, "admin-2", "admin2@example.com", auth.RoleAdmin)
	bearer := adminBearer(t, rt, "admin-1", "admin1@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost,
		"/api/admin/users/admin-2/role", bearer,
		map[string]any{"role": string(auth.RoleMember)},
	)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 when demoting one of two admins, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	u, err := rt.app.StorageBackend().LoadUserByID(context.Background(), "admin-2")
	if err != nil {
		t.Fatalf("LoadUserByID: %v", err)
	}
	if u.Role != auth.RoleMember {
		t.Errorf("expected admin-2 demoted to member, got %q", u.Role)
	}
}

// TestAdminUserRole_PromotingMember_AlwaysSucceeds confirms the safeguard
// only blocks demotions of admins — member→admin (and admin→admin) are
// always allowed.
func TestAdminUserRole_PromotingMember_AlwaysSucceeds(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	seedUserWithRole(t, rt, "member-1", "member@example.com", auth.RoleMember)
	bearer := adminBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost,
		"/api/admin/users/member-1/role", bearer,
		map[string]any{"role": string(auth.RoleAdmin)},
	)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 when promoting member, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}
