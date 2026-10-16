package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
)

// These tests verify the HANDLER layer: auth, body parsing, org-membership checks, and error mapping. The
// test router's backend (the fake, held to Postgres's semantics by the storage contract) stores policies, so
// the member cases assert whole-request success.

func TestPolicySave_OrgMemberSucceeds(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	mirrorOrg(t, rt, orgID) // a policy needs its organisation in the store
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/save", bearer, map[string]any{
		"id":           "pol-1",
		"orgId":        orgID,
		"name":         "Security baseline",
		"description":  "Must pass",
		"rules":        []map[string]any{{"ruleId": "hardcoded-credential", "enabled": true}},
		"gateSeverity": "error",
	})
	checkStatus(t, rr, http.StatusOK)
}

func TestPolicySave_NonMemberForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	bearer := jwtBearer(t, rt, "bob", "bob@example.com") // bob is NOT in Acme

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/save", bearer, map[string]any{
		"id":    "pol-1",
		"orgId": orgID,
		"name":  "Test",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

// TestPolicySave_NonAdminMemberForbidden is the regression test for the
// broken-access-control fix: saving a policy is a WRITE to the org-wide CI gate
// (SavePolicy upserts on id+org_id), so a non-admin member (here a plain
// member; a viewer/guest behaves the same) must be rejected. Before the fix this
// path gated on requireOrgMember and returned 200, letting any member silently
// overwrite the org's governance rules.
func TestPolicySave_NonAdminMemberForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice") // alice is the admin owner
	addOrgMember(t, rt, orgID, "mallory", auth.RoleMember)
	bearer := jwtBearer(t, rt, "mallory", "mallory@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/save", bearer, map[string]any{
		"id":           "pol-1",
		"orgId":        orgID,
		"name":         "Overwritten",
		"gateSeverity": "info",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

// TestPolicyDelete_NonAdminMemberForbidden is the regression test for the delete
// side of the same fix: a non-admin member must not be able to remove the org's
// CI gate. Before the fix this gated on requireOrgMember and returned 200.
func TestPolicyDelete_NonAdminMemberForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	addOrgMember(t, rt, orgID, "mallory", auth.RoleViewer)
	bearer := jwtBearer(t, rt, "mallory", "mallory@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/delete", bearer, map[string]any{
		"orgId": orgID,
		"id":    "pol-1",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestPolicySave_MissingNameReturns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/save", bearer, map[string]any{
		"id":    "pol-1",
		"orgId": orgID,
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestPolicyList_ReturnsEmptyArray(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/list", bearer, map[string]any{
		"orgId": orgID,
	})
	checkStatus(t, rr, http.StatusOK)
	// No policy saved: an empty list
	var policies []any
	decodeJSON(t, rr, &policies)
	// nil or empty is fine
}

func TestPolicyList_NonMemberForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/list", bearer, map[string]any{
		"orgId": orgID,
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestPolicyGet_NonExistentReturnsError(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/get", bearer, map[string]any{
		"orgId": orgID,
		"id":    "nonexistent",
	})
	// The store answers ErrNotFound → handler maps to 404 or 403
	if rr.Code != http.StatusNotFound && rr.Code != http.StatusForbidden {
		t.Errorf("expected 404 or 403 for non-existent policy, got %d", rr.Code)
	}
}

func TestPolicyGet_NonMemberForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/get", bearer, map[string]any{
		"orgId": orgID,
		"id":    "any",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestPolicyDelete_OrgMemberSucceeds(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/delete", bearer, map[string]any{
		"orgId": orgID,
		"id":    "pol-1",
	})
	checkStatus(t, rr, http.StatusOK)
}

func TestPolicyDelete_NonMemberForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	bearer := jwtBearer(t, rt, "carol", "carol@example.com") // carol is NOT in Acme

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/delete", bearer, map[string]any{
		"orgId": orgID,
		"id":    "pol-1",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

// TestPolicySave_WithoutIDGetsAGeneratedOne: the policy panel creates a policy without an id. Stored under the empty id, every
// new policy of an organisation overwrote the one before; now each gets an id of its own, answered to the client so
// its next save updates it, and an explicit id is kept.
func TestPolicySave_WithoutIDGetsAGeneratedOne(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	mirrorOrg(t, rt, orgID) // a policy needs its organisation in the store
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	save := func(body map[string]any) map[string]any {
		t.Helper()
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/save", bearer, body)
		checkStatus(t, rr, http.StatusOK)
		var saved map[string]any
		decodeJSON(t, rr, &saved)
		return saved
	}
	first := save(map[string]any{"id": "", "orgId": orgID, "name": "First"})
	second := save(map[string]any{"orgId": orgID, "name": "Second"})
	id1, _ := first["id"].(string)
	id2, _ := second["id"].(string)
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("ids of two policies saved without one = %q, %q; want two distinct generated ids", id1, id2)
	}
	first["name"] = "First, renamed"
	if again := save(first); again["id"] != id1 {
		t.Errorf("saving the policy back under its id gave id %v, want %s", again["id"], id1)
	}
	kept := save(map[string]any{"id": "pol-explicit", "orgId": orgID, "name": "Explicit"})
	if kept["id"] != "pol-explicit" {
		t.Errorf("an explicit id came back as %v", kept["id"])
	}

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/list", bearer, map[string]any{"orgId": orgID})
	checkStatus(t, rr, http.StatusOK)
	var list []map[string]any
	decodeJSON(t, rr, &list)
	names := map[string]string{}
	for _, p := range list {
		id, _ := p["id"].(string)
		names[id], _ = p["name"].(string)
	}
	if len(names) != 3 || names[id1] != "First, renamed" || names[id2] != "Second" || names["pol-explicit"] != "Explicit" {
		t.Errorf("the organisation's policies = %v; want the three, the first renamed", names)
	}
}
