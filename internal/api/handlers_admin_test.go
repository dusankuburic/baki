package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
)

// TestAdminUserRole_DemotingLastAdmin_Returns409 verifies the N-5 safeguard:
// when an admin tries to demote themselves (or any admin) while they are the
// only admin in the system, the request is refused with 409 Conflict and the
// role is left unchanged. Without this, a one-admin instance could be locked
// out of admin functions entirely.
func TestAdminUserRole_DemotingLastAdmin_Returns409(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut,
		"/api/admin/users/admin-1/role", bearer,
		map[string]any{"role": string(auth.RoleMember)},
	)
	if rr.Code != http.StatusConflict {
		t.Errorf("expected 409 when demoting last admin, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	// Verify the role was not changed.
	u, err := rt.security.Backend.LoadUserByID(context.Background(), "admin-1")
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
	bearer := jwtBearer(t, rt, "admin-1", "admin1@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut,
		"/api/admin/users/admin-2/role", bearer,
		map[string]any{"role": string(auth.RoleMember)},
	)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 when demoting one of two admins, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	u, err := rt.security.Backend.LoadUserByID(context.Background(), "admin-2")
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
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut,
		"/api/admin/users/member-1/role", bearer,
		map[string]any{"role": string(auth.RoleAdmin)},
	)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 when promoting member, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestMigrationStart_Unconfigured_Returns503: in the test router the migration
// runner is disabled (no PAD_STORAGE_DATA_DIR), so the admin endpoint must
// report 503 rather than the old hardcoded 501.
func TestMigrationStart_Unconfigured_Returns503(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/migration/start", bearer, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when migration unconfigured, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestMigrationStatus_Unconfigured reports configured=false.
func TestMigrationStatus_Unconfigured(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/migration/status", bearer, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var st map[string]any
	decodeJSON(t, rr, &st)
	if st["configured"] != false {
		t.Errorf("expected configured=false, got %v", st["configured"])
	}
	if st["running"] != false {
		t.Errorf("expected running=false, got %v", st["running"])
	}
}

// TestMigrationStart_NonAdmin_Forbidden proves the admin-only gate applies to
// the migration endpoint (a member must not trigger migration).
func TestMigrationStart_NonAdmin_Forbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	seedUserWithRole(t, rt, "member-1", "member@example.com", auth.RoleMember)
	// Issue a MEMBER-scoped token (jwtBearer always mints admin).
	pair, err := rt.security.AuthMgr.Issue("member-1", "member@example.com", auth.RoleMember)
	if err != nil {
		t.Fatalf("issue member jwt: %v", err)
	}
	bearer := "Bearer " + pair.AccessToken

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/migration/start", bearer, nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestPPAuth_Start_Unconfigured_Returns503 verifies the nil-gate: when the
// connector isn't configured (nil PadCloudAuth) the start endpoint reports 503.
func TestPPAuth_Start_Unconfigured_Returns503(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/powerplatform/start", bearer, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when PP auth unconfigured, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestPPAuth_Poll_Unconfigured_Returns503 mirrors the above for the poll path.
func TestPPAuth_Poll_Unconfigured_Returns503(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/powerplatform/poll", bearer,
		map[string]any{"deviceCode": "test"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when PP auth unconfigured, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestPPAuth_Status_ReturnsConnectedFalse verifies the status endpoint reports
// connected=false when no token is cached (the test router has nil ppAuth).
func TestPPAuth_Status_ReturnsConnectedFalse(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/powerplatform/status", bearer, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var st map[string]any
	decodeJSON(t, rr, &st)
	if st["connected"] != false {
		t.Errorf("expected connected=false, got %v", st["connected"])
	}
}

// TestPPAuth_NonAdmin_Forbidden proves the admin-only gate applies to the PP
// auth endpoints.
func TestPPAuth_NonAdmin_Forbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	seedUserWithRole(t, rt, "member-1", "member@example.com", auth.RoleMember)
	pair, err := rt.security.AuthMgr.Issue("member-1", "member@example.com", auth.RoleMember)
	if err != nil {
		t.Fatalf("issue member jwt: %v", err)
	}
	bearer := "Bearer " + pair.AccessToken

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/powerplatform/start", bearer, nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestScannerScan_Unconfigured_Returns503 verifies the nil-gate: when the
// scanner isn't cloud-configured (nil ScanNowFunc) the manual trigger reports
// 503, matching the PadCloud-auth pattern.
func TestScannerScan_Unconfigured_Returns503(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/scanner/scan", bearer, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when scanner unconfigured, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestIngesterIngest_Unconfigured_Returns503 mirrors the above for the ingest
// trigger.
func TestIngesterIngest_Unconfigured_Returns503(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/ingester/ingest", bearer, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when ingester unconfigured, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestScannerScan_Configured_TriggersAndReturnsStarted verifies that when a
// ScanNowFunc is wired, the endpoint returns {started: true} and actually
// invokes the function (on a detached goroutine). The test router wires nil by
// default; we mutate the Admin handler's field directly (in-package) — the
// registered route reads it at call time off the same *AdminHandler pointer.
func TestScannerScan_Configured_TriggersAndReturnsStarted(t *testing.T) {
	triggered := make(chan struct{}, 1)
	rt := newJWTTestRouter(t)
	rt.handlers.Admin.scanNow = func(context.Context) {
		triggered <- struct{}{}
	}
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/scanner/scan", bearer, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["started"] != true {
		t.Errorf("expected started=true, got %v", resp["started"])
	}
	select {
	case <-triggered:
		// the wired func ran
	case <-time.After(2 * time.Second):
		t.Fatal("ScanNowFunc was not invoked")
	}
}

// TestScannerScan_NonAdmin_Forbidden proves the admin gate applies.
func TestScannerScan_NonAdmin_Forbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	seedUserWithRole(t, rt, "member-1", "member@example.com", auth.RoleMember)
	pair, err := rt.security.AuthMgr.Issue("member-1", "member@example.com", auth.RoleMember)
	if err != nil {
		t.Fatalf("issue member jwt: %v", err)
	}
	bearer := "Bearer " + pair.AccessToken

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/scanner/scan", bearer, nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}
