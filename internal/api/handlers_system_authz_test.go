package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/testutil"
)

func newOrgSettingsRouter(t *testing.T) *Router {
	t.Helper()
	store := testutil.NewFakeBackend()
	return newTestRouter(store, true)
}

func TestOrgSettingsGet_NonMemberForbidden(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/system/settings/org/"+orgID, bearer, nil)
	checkStatus(t, rr, http.StatusForbidden)
}

func TestOrgSettingsGet_MemberAllowed(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	addOrgMember(t, rt, orgID, "bob", auth.RoleViewer)
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/system/settings/org/"+orgID, bearer, nil)
	checkStatus(t, rr, http.StatusOK)
}

func TestOrgSettingsUpdate_MemberForbidden(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	addOrgMember(t, rt, orgID, "bob", auth.RoleMember)
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, bearer, map[string]any{})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestOrgSettingsUpdate_AdminAllowed(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	mirrorOrg(t, rt, orgID)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, bearer, map[string]any{})
	checkStatus(t, rr, http.StatusOK)
}

func TestOrgSettings_LocalModeBypass(t *testing.T) {
	// Local mode (no JWT): org checks short-circuit; settings fall back to the
	// global store rather than 403ing the single desktop user.
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodGet, "/api/system/settings/org/any-org", nil)
	checkStatus(t, rr, http.StatusOK)
}
