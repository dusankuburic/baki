package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/filesystem"
)

// createOrg creates an org via the API and returns its ID.
// The owner is always the router's local user ("local" in test routers).
func createOrg(t *testing.T, rt *Router, name string) string {
	t.Helper()
	rr := doRequest(t, rt, http.MethodPost, "/api/orgs", map[string]any{
		"name": name,
	})
	checkStatus(t, rr, http.StatusOK)
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatal("createOrg: response missing id")
	}
	return id
}

func TestHandleOrgList_EmptyByDefault(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodGet, "/api/orgs", nil)
	checkStatus(t, rr, http.StatusOK)

	var orgs []any
	decodeJSON(t, rr, &orgs)
	if len(orgs) != 0 {
		t.Errorf("expected empty list, got %d items", len(orgs))
	}
}

func TestHandleOrgList_ShowsOrgAfterCreate(t *testing.T) {
	rt := newTestRouter(nil, false)
	createOrg(t, rt, "Acme")

	rr := doRequest(t, rt, http.MethodGet, "/api/orgs", nil)
	checkStatus(t, rr, http.StatusOK)

	var orgs []any
	decodeJSON(t, rr, &orgs)
	if len(orgs) != 1 {
		t.Errorf("expected 1 org, got %d", len(orgs))
	}
}

func TestHandleOrgCreate_OK(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/orgs", map[string]any{
		"name": "Acme",
	})
	checkStatus(t, rr, http.StatusOK)

	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["id"] == nil {
		t.Error("response missing id")
	}
	if resp["name"] != "Acme" {
		t.Errorf("expected name=Acme, got %v", resp["name"])
	}
	// Owner is always the authenticated caller (localUserID="local" in test routers).
	if resp["ownerId"] != "local" {
		t.Errorf("expected ownerId=local, got %v", resp["ownerId"])
	}
}

func TestHandleOrgCreate_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/orgs")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleOrgDelete_OK(t *testing.T) {
	rt := newTestRouter(nil, false)
	orgID := createOrg(t, rt, "Acme")

	rr := doRequest(t, rt, http.MethodDelete, "/api/orgs/"+orgID, nil)
	checkStatus(t, rr, http.StatusOK)

	// The org list should now be empty.
	rr = doRequest(t, rt, http.MethodGet, "/api/orgs", nil)
	var orgs []any
	decodeJSON(t, rr, &orgs)
	if len(orgs) != 0 {
		t.Errorf("expected 0 orgs after delete, got %d", len(orgs))
	}
}

func TestHandleOrgMemberRemove_OK(t *testing.T) {
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	rt := newTestRouter(fs, false)
	orgID := createOrg(t, rt, "Team")
	seedUserWithRole(t, rt, "member1", "member1@example.com", "member")

	addOrgMember(t, rt, orgID, "member1", auth.RoleMember)

	rr := doRequest(t, rt, http.MethodDelete, "/api/orgs/"+orgID+"/members/member1", nil)
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleOrgMemberRemove_LastAdminReturns409(t *testing.T) {
	rt := newTestRouter(nil, false)
	orgID := createOrg(t, rt, "Team")

	// "local" is the owner/only admin — removing them should be blocked.
	rr := doRequest(t, rt, http.MethodDelete, "/api/orgs/"+orgID+"/members/local", nil)
	checkStatus(t, rr, http.StatusConflict)
}

func TestHandleOrgMemberRemove_UnknownOrgReturns404(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodDelete, "/api/orgs/no-such-org/members/member1", nil)
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleOrgMemberSetRole_OK(t *testing.T) {
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	rt := newTestRouter(fs, false)
	orgID := createOrg(t, rt, "Team")
	seedUserWithRole(t, rt, "member1", "member1@example.com", "member")

	addOrgMember(t, rt, orgID, "member1", auth.RoleMember)

	rr := doRequest(t, rt, http.MethodPut, "/api/orgs/"+orgID+"/members/member1/role", map[string]any{
		"role": "admin",
	})
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleOrgMemberSetRole_UnknownOrgReturns404(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPut, "/api/orgs/no-such-org/members/member1/role", map[string]any{
		"role": "admin",
	})
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleOrgMemberSetRole_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	orgID := "some-org"
	rr := newBadBodyRequest(t, rt, http.MethodPut, "/api/orgs/"+orgID+"/members/member1/role")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleOrgUpdate_RenamesForAdmin(t *testing.T) {
	rt := newTestRouter(nil, false)
	orgID := createOrg(t, rt, "Acme")

	rr := doRequest(t, rt, http.MethodPut, "/api/orgs/"+orgID, map[string]any{"name": "  Acme Corp  "})
	checkStatus(t, rr, http.StatusOK)
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["name"] != "Acme Corp" {
		t.Errorf("expected trimmed name %q, got %v", "Acme Corp", resp["name"])
	}
}

// Create refuses an empty name, so rename must as well — otherwise an org can
// be renamed into a blank row in the switcher.
func TestHandleOrgUpdate_BlankNameReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	orgID := createOrg(t, rt, "Acme")

	rr := doRequest(t, rt, http.MethodPut, "/api/orgs/"+orgID, map[string]any{"name": "   "})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleOrgUpdate_NonAdminForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "boss", "boss@acme.io", auth.RoleMember)
	seedUserWithRole(t, rt, "dev", "dev@acme.io", auth.RoleMember)
	orgID := seedOrg(t, rt, "Acme", "boss")
	addOrgMember(t, rt, orgID, "dev", auth.RoleMember)

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/orgs/"+orgID, jwtBearer(t, rt, "dev", "dev@acme.io"),
		map[string]any{"name": "Hijacked"})
	checkStatus(t, rr, http.StatusForbidden)
}

// The settings panel's "Leave organisation" button calls the member-remove
// route on the caller's own row. That only works because the handler lets a
// non-admin remove themselves while still refusing to let them remove anyone
// else.
func TestHandleOrgMemberRemove_NonAdminMayLeaveButNotRemoveOthers(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "boss", "boss@acme.io", auth.RoleMember)
	seedUserWithRole(t, rt, "dev", "dev@acme.io", auth.RoleMember)
	seedUserWithRole(t, rt, "qa", "qa@acme.io", auth.RoleMember)
	orgID := seedOrg(t, rt, "Acme", "boss")
	addOrgMember(t, rt, orgID, "dev", auth.RoleMember)
	addOrgMember(t, rt, orgID, "qa", auth.RoleMember)
	dev := jwtBearer(t, rt, "dev", "dev@acme.io")

	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/orgs/"+orgID+"/members/qa", dev, nil)
	checkStatus(t, rr, http.StatusForbidden)

	rr = doRequestWithAuth(t, rt, http.MethodDelete, "/api/orgs/"+orgID+"/members/dev", dev, nil)
	checkStatus(t, rr, http.StatusOK)

	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/orgs", dev, nil)
	checkStatus(t, rr, http.StatusOK)
	var orgs []any
	decodeJSON(t, rr, &orgs)
	if len(orgs) != 0 {
		t.Errorf("expected the org to drop out of the leaver's list, got %d orgs", len(orgs))
	}
}
