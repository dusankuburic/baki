package api

import (
	"net/http"
	"testing"
)

// createOrg creates an org via the API and returns its ID.
// The owner is always the router's local user ("local" in test routers).
func createOrg(t *testing.T, rt *Router, name string) string {
	t.Helper()
	rr := doRequest(t, rt, http.MethodPost, "/api/org/create", map[string]any{
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
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/org/list", nil)
	checkStatus(t, rr, http.StatusOK)

	var orgs []any
	decodeJSON(t, rr, &orgs)
	if len(orgs) != 0 {
		t.Errorf("expected empty list, got %d items", len(orgs))
	}
}

func TestHandleOrgList_ShowsOrgAfterCreate(t *testing.T) {
	rt := newTestRouter()
	createOrg(t, rt, "Acme")

	rr := doRequest(t, rt, http.MethodPost, "/api/org/list", nil)
	checkStatus(t, rr, http.StatusOK)

	var orgs []any
	decodeJSON(t, rr, &orgs)
	if len(orgs) != 1 {
		t.Errorf("expected 1 org, got %d", len(orgs))
	}
}

func TestHandleOrgCreate_OK(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/org/create", map[string]any{
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
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/org/create")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleOrgMemberAdd_OK(t *testing.T) {
	rt := newTestRouter()
	orgID := createOrg(t, rt, "Team")

	rr := doRequest(t, rt, http.MethodPost, "/api/org/member/add", map[string]any{
		"orgId":  orgID,
		"userId": "member1",
		"role":   "member",
	})
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleOrgMemberAdd_UnknownOrgReturns404(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/org/member/add", map[string]any{
		"orgId":  "no-such-org",
		"userId": "member1",
		"role":   "member",
	})
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleOrgMemberAdd_DuplicateReturns409(t *testing.T) {
	rt := newTestRouter()
	orgID := createOrg(t, rt, "Team")

	addReq := map[string]any{"orgId": orgID, "userId": "member1", "role": "member"}
	doRequest(t, rt, http.MethodPost, "/api/org/member/add", addReq)

	// Adding the same member again should return 409 Conflict.
	rr := doRequest(t, rt, http.MethodPost, "/api/org/member/add", addReq)
	checkStatus(t, rr, http.StatusConflict)
}

func TestHandleOrgMemberAdd_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/org/member/add")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleOrgMemberRemove_OK(t *testing.T) {
	rt := newTestRouter()
	orgID := createOrg(t, rt, "Team")
	doRequest(t, rt, http.MethodPost, "/api/org/member/add", map[string]any{
		"orgId": orgID, "userId": "member1", "role": "member",
	})

	rr := doRequest(t, rt, http.MethodPost, "/api/org/member/remove", map[string]any{
		"orgId": orgID, "userId": "member1",
	})
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleOrgMemberRemove_LastAdminReturns409(t *testing.T) {
	rt := newTestRouter()
	orgID := createOrg(t, rt, "Team")

	// "local" is the owner/only admin — removing them should be blocked.
	rr := doRequest(t, rt, http.MethodPost, "/api/org/member/remove", map[string]any{
		"orgId": orgID, "userId": "local",
	})
	checkStatus(t, rr, http.StatusConflict)
}

func TestHandleOrgMemberRemove_UnknownOrgReturns404(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/org/member/remove", map[string]any{
		"orgId": "no-such-org", "userId": "member1",
	})
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleOrgMemberRemove_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/org/member/remove")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleOrgMemberSetRole_OK(t *testing.T) {
	rt := newTestRouter()
	orgID := createOrg(t, rt, "Team")
	doRequest(t, rt, http.MethodPost, "/api/org/member/add", map[string]any{
		"orgId": orgID, "userId": "member1", "role": "member",
	})

	rr := doRequest(t, rt, http.MethodPost, "/api/org/member/role", map[string]any{
		"orgId": orgID, "userId": "member1", "role": "admin",
	})
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleOrgMemberSetRole_UnknownOrgReturns404(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/org/member/role", map[string]any{
		"orgId": "no-such-org", "userId": "member1", "role": "admin",
	})
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleOrgMemberSetRole_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/org/member/role")
	checkStatus(t, rr, http.StatusBadRequest)
}
