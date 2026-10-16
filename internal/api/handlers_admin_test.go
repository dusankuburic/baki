package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
)

// TestAdminUserList_FiltersAndPages pins the paged envelope the admin table
// reads: limit/offset page the list, total counts the users the filters match
// (not the page), q searches email and display name, role narrows to one
// role, and an empty page is [] rather than null.
func TestAdminUserList_FiltersAndPages(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	for i := range 11 {
		seedUserWithRole(t, rt, fmt.Sprintf("user-%02d", i), fmt.Sprintf("user-%02d@example.com", i), auth.RoleMember)
	}
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	type page struct {
		Items  []map[string]any `json:"items"`
		Total  int              `json:"total"`
		Offset int              `json:"offset"`
		Limit  int              `json:"limit"`
	}
	get := func(query string) (page, *httptest.ResponseRecorder) {
		t.Helper()
		rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list"+query, bearer, nil)
		var p page
		if rr.Code == http.StatusOK {
			decodeJSON(t, rr, &p)
		}
		return p, rr
	}

	p, rr := get("?limit=10&offset=0")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if len(p.Items) != 10 || p.Total != 12 || p.Limit != 10 || p.Offset != 0 {
		t.Errorf("page 1: expected 10 of 12 (limit 10), got %d of %d (limit %d, offset %d)", len(p.Items), p.Total, p.Limit, p.Offset)
	}
	p, _ = get("?limit=10&offset=10")
	if len(p.Items) != 2 || p.Total != 12 {
		t.Errorf("page 2: expected 2 of 12, got %d of %d", len(p.Items), p.Total)
	}

	// Past the end: [] on the wire, never null (read raw, before any decode).
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list?limit=10&offset=100", bearer, nil)
	if !strings.Contains(rr.Body.String(), `"items":[]`) {
		t.Errorf("empty page: expected \"items\":[] in %s", rr.Body.String())
	}

	p, _ = get("?q=USER-03")
	if p.Total != 1 || len(p.Items) != 1 || p.Items[0]["email"] != "user-03@example.com" {
		t.Errorf("q=USER-03: expected only user-03@example.com, got %v (total %d)", p.Items, p.Total)
	}
	p, _ = get("?role=member")
	if p.Total != 11 {
		t.Errorf("role=member: expected total 11, got %d", p.Total)
	}
	p, _ = get("?role=admin&limit=5")
	if p.Total != 1 || len(p.Items) != 1 {
		t.Errorf("role=admin: expected 1 of 1, got %d of %d", len(p.Items), p.Total)
	}
	p, _ = get("?limit=1000")
	if p.Limit != 100 {
		t.Errorf("limit=1000: expected the limit clamped to 100, got %d", p.Limit)
	}

	if _, rr := get("?role=bogus"); rr.Code != http.StatusBadRequest {
		t.Errorf("role=bogus: expected 400, got %d", rr.Code)
	}
	if _, rr := get("?q=" + strings.Repeat("a", 201)); rr.Code != http.StatusBadRequest {
		t.Errorf("q of 201 characters: expected 400, got %d", rr.Code)
	}
}

// TestAdminUserStats counts the whole table by role and state, including
// accounts with no valid role ("none"), and is admin-only.
func TestAdminUserStats(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	seedUserWithRole(t, rt, "member-1", "m1@example.com", auth.RoleMember)
	seedUserWithRole(t, rt, "member-2", "m2@example.com", auth.RoleMember)
	seedUserWithRole(t, rt, "viewer-1", "v1@example.com", auth.RoleViewer)
	seedUserWithRole(t, rt, "roleless-1", "none@example.com", "")
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/stats", bearer, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("stats: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var stats struct {
		Total  int `json:"total"`
		ByRole struct {
			Admin, Member, Viewer, Guest, None int
		} `json:"byRole"`
		Unverified int `json:"unverified"`
		Locked     int `json:"locked"`
	}
	decodeJSON(t, rr, &stats) // encoding/json matches the lowercase keys case-insensitively
	if stats.Total != 5 || stats.ByRole.Admin != 1 || stats.ByRole.Member != 2 || stats.ByRole.Viewer != 1 ||
		stats.ByRole.Guest != 0 || stats.ByRole.None != 1 {
		t.Errorf("unexpected counts: %+v", stats)
	}
	if stats.Unverified != 5 || stats.Locked != 0 {
		t.Errorf("seeded users are unverified and unlocked: got unverified %d, locked %d", stats.Unverified, stats.Locked)
	}

	memberPair, err := rt.security.AuthMgr.Issue("member-1", "m1@example.com", auth.RoleMember)
	if err != nil {
		t.Fatalf("issue member jwt: %v", err)
	}
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/stats", "Bearer "+memberPair.AccessToken, nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("stats as member: expected 403, got %d", rr.Code)
	}
}

// TestAdminUserRole_UnknownUser_Returns404: a role change for an id that does
// not exist is reported, not silently acknowledged (and audited).
func TestAdminUserRole_UnknownUser_Returns404(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/admin/users/nobody/role", bearer,
		map[string]any{"role": string(auth.RoleMember)})
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for an unknown user, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

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
// runner is disabled (no PAD_DATA_DIR), so the admin endpoint must
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

// TestScannerScan_Unconfigured_Returns503 verifies the nil-gate: when the
// scanner isn't cloud-configured (nil ScanNowFunc) the manual trigger reports
// 503.
func TestScannerScan_Unconfigured_Returns503(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/scanner/scan", bearer, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when scanner unconfigured, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestScannerScan_Configured_TriggersAndReturnsStarted verifies that when a
// ScanNowFunc is wired, the endpoint returns {started: true} once it reports the
// sweep started. The test router wires nil by default; we mutate the Admin
// handler's field directly (in-package) — the registered route reads it at call
// time off the same *AdminHandler pointer.
func TestScannerScan_Configured_TriggersAndReturnsStarted(t *testing.T) {
	triggered := make(chan struct{}, 1)
	rt := newJWTTestRouter(t)
	moduleOf[*AdminHandler](t, rt).scanNow = func(context.Context) error {
		triggered <- struct{}{}
		return nil
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

// testAdminScanJob stands in for the scanner's job: the admin trigger starts it as the scanner's ScanNow does.
var testAdminScanJob = lifecycle.DeclareJob(lifecycle.JobDecl{Name: "test_admin_scan", Schedule: "tests only"})

// TestScannerScan_ReportsTheSweepsOutcome: the answer says what became of the sweep. It used to be {started: true}
// whenever the request reached the handler, also when the sweep then skipped because one ran on another replica, the
// lock could not be asked for, or the scanner had stopped.
func TestScannerScan_ReportsTheSweepsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		locker  *fakeLocker
		stopped bool
		status  int
		code    string
	}{
		{"lock free: started", &fakeLocker{acquired: true, released: make(chan struct{})}, false, http.StatusOK, ""},
		{"a sweep runs on another replica", &fakeLocker{}, false, http.StatusConflict, "SCAN_ALREADY_RUNNING"},
		{"the lock cannot be asked for", &fakeLocker{err: errors.New("connection refused")}, false, http.StatusServiceUnavailable, "SCAN_UNAVAILABLE"},
		{"the scanner has stopped", &fakeLocker{acquired: true, released: make(chan struct{})}, true, http.StatusServiceUnavailable, "SERVER_RESTARTING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var runs atomic.Int32
			ran := make(chan struct{}, 1)
			job := lifecycle.NewSingleton(lifecycle.Job{Spec: testAdminScanJob, Run: func(context.Context) error {
				runs.Add(1)
				ran <- struct{}{}
				return nil
			}}, lock.AsLeaser(tc.locker), nil)
			t.Cleanup(job.Stop)
			if tc.stopped {
				job.Stop()
			}
			rt := newJWTTestRouter(t)
			moduleOf[*AdminHandler](t, rt).scanNow = job.RunNow
			seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
			bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

			rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/scanner/scan", bearer, nil)
			if rr.Code != tc.status {
				t.Fatalf("status %d (body: %s), want %d", rr.Code, rr.Body.String(), tc.status)
			}
			var resp map[string]any
			decodeJSON(t, rr, &resp)
			if tc.code == "" {
				if resp["started"] != true {
					t.Errorf("body %v, want started=true", resp)
				}
				select {
				case <-ran:
				case <-time.After(5 * time.Second):
					t.Fatal("the sweep it reported started never ran")
				}
				return
			}
			if resp["code"] != tc.code {
				t.Errorf("code %v (body %v), want %s", resp["code"], resp, tc.code)
			}
			if n := runs.Load(); n != 0 {
				t.Errorf("the sweep ran %d time(s), want none", n)
			}
		})
	}
}
