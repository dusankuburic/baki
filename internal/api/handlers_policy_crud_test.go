package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
)

// The filesystem backend does not store policies: Get→ErrNotFound, List→empty, and Save/Delete
// return ErrPoliciesUnsupported. They previously returned nil — SUCCESS — while writing nothing,
// which is why the two "…Succeeds" tests below used to assert a flat 200 off a backend that
// cannot store a policy at all. These tests verify the HANDLER layer: auth, body parsing,
// org-membership checks, and error mapping — so the member cases assert what they mean
// (authorization passed), not whole-request success, which depends on the backend.

func TestPolicySave_OrgMemberSucceeds(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/save", bearer, map[string]any{
		"id":           "pol-1",
		"orgId":        orgID,
		"name":         "Security baseline",
		"description":  "Must pass",
		"rules":        []map[string]any{{"ruleId": "hardcoded-credential", "enabled": true}},
		"gateSeverity": "error",
	})
	// Authorization passed. Persistence is a backend concern: this router is
	// backed by the filesystem store, which cannot hold policies.
	if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
		t.Fatalf("an org member was denied: status %d — body: %s", rr.Code, rr.Body.String())
	}
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
	// Filesystem backend returns empty list
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
	// Filesystem backend returns ErrNotFound → handler maps to 404 or 403
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
	// Authorization passed; see the note at the top of this file on why this is
	// not a flat 200 assertion.
	if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
		t.Fatalf("an org member was denied: status %d — body: %s", rr.Code, rr.Body.String())
	}
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
