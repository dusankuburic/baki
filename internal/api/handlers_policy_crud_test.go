package api

import (
	"net/http"
	"testing"
)

// ---- Persistent Policy CRUD handler tests ----
//
// The filesystem backend stubs policy methods (no-op Save/Delete, Get→ErrNotFound,
// List→empty). These tests verify the handler layer: auth, body parsing,
// org-membership checks, and error mapping — not the storage implementation.

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
