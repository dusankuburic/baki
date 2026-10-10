package api

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
)

var updateAuthzMatrix = flag.Bool("update-authz-matrix", false, "rewrite testdata/authz_matrix.golden")

// authzIdentity is one caller of the matrix: an interactive session with a global role, a scoped token, or nobody.
type authzIdentity struct {
	name   string
	user   string    // "" = anonymous
	role   auth.Role // global role of user
	scopes []string  // non-nil = a PAT with these scopes instead of a session
}

// authzEndpoint is one request of the matrix; {org}, {mine} and {orgflow} are filled per environment.
type authzEndpoint struct {
	method, path string
	body         any
}

// TestAuthzMatrix_Golden records the status every kind of caller gets from a representative endpoint of each
// route group: anonymous, a global viewer / member / admin session, a member who owns the resources, a member
// outside the org, and scoped tokens. Phase 1a moves authorization from handler bodies onto route groups; this
// file must not change while it does. Refresh only for an intended change:
// go test ./internal/api/ -run TestAuthzMatrix_Golden -update-authz-matrix
func TestAuthzMatrix_Golden(t *testing.T) {
	identities := []authzIdentity{
		{name: "anonymous"},
		{name: "viewer", user: "u-viewer", role: auth.RoleViewer},
		{name: "owner", user: "u-owner", role: auth.RoleMember},
		{name: "outsider", user: "u-outsider", role: auth.RoleMember},
		{name: "admin", user: "u-admin", role: auth.RoleAdmin},
		{name: "pat:read", user: "u-owner", role: auth.RoleMember, scopes: []string{auth.ScopeRead}},
		{name: "pat:read+write", user: "u-owner", role: auth.RoleMember, scopes: []string{auth.ScopeRead, auth.ScopeWrite}},
		{name: "pat:chat", user: "u-owner", role: auth.RoleMember, scopes: []string{auth.ScopeChat}},
		{name: "pat:chat+write", user: "u-owner", role: auth.RoleMember, scopes: []string{auth.ScopeChat, auth.ScopeWrite}},
		{name: "pat:admin", user: "u-admin", role: auth.RoleAdmin, scopes: []string{auth.ScopeAdmin}},
		{name: "pat:ci", user: "u-owner", role: auth.RoleMember, scopes: []string{auth.ScopeCI}},
	}
	endpoints := []authzEndpoint{
		{http.MethodGet, "/api/auth/me", nil},
		{http.MethodGet, "/api/auth/tokens", nil},
		{http.MethodGet, "/api/auth/sessions", nil},
		{http.MethodPost, "/api/auth/tokens", map[string]any{"name": "matrix"}},
		{http.MethodPost, "/api/ws-ticket", nil},
		{http.MethodGet, "/api/admin/users/list", nil},
		{http.MethodGet, "/api/admin/audit", nil},
		{http.MethodGet, "/api/admin/system/health", nil},
		{http.MethodGet, "/api/library", nil},
		{http.MethodGet, "/api/library/{mine}", nil},
		{http.MethodGet, "/api/library/{orgflow}/content", nil},
		{http.MethodGet, "/api/library/portfolio", nil},
		{http.MethodGet, "/api/flows/{orgflow}/collaborators", nil},
		{http.MethodGet, "/api/dashboard/home", nil},
		{http.MethodGet, "/api/governance/alerts", nil},
		{http.MethodGet, "/api/system/settings", nil},
		{http.MethodGet, "/api/system/settings/org/{org}", nil},
		{http.MethodGet, "/api/providers/list", nil},
		{http.MethodGet, "/api/providers/catalog", nil},
		{http.MethodGet, "/api/orgs/{org}/channels", nil},
		{http.MethodGet, "/api/orgs/{org}/channels/options", nil},
		{http.MethodPost, "/api/orgs/{org}/channels", map[string]any{"kind": "webhook", "url": "https://hooks.example.com/x"}},
		{http.MethodDelete, "/api/orgs/{org}/channels/no-such-channel", nil},
		{http.MethodGet, "/api/orgs/{org}/ci-credentials", nil},
		{http.MethodPost, "/api/orgs/{org}/ci-credentials", map[string]any{"name": "matrix"}},
		{http.MethodDelete, "/api/orgs/{org}/ci-credentials/no-such-credential", nil},
		{http.MethodGet, "/api/orgs/{org}/rules/export", nil},
		{http.MethodPost, "/api/analysis/triage/list", map[string]any{"flowId": "{orgflow}"}},
		{http.MethodPost, "/api/analysis/comments/list", map[string]any{"flowId": "{orgflow}", "findingKey": "k-1"}},
		{http.MethodPost, "/api/chat/fix-decision", map[string]any{"streamId": "s-1", "proposalId": "p-1", "approved": true}},
		{http.MethodPost, "/api/admin/scanner/scan", map[string]any{}},
		{http.MethodPost, "/api/integrations/ci", map[string]any{"flowId": "{orgflow}"}},
	}

	var b strings.Builder
	for _, id := range identities {
		rt, seedFlow := newLibraryTestRouter(t)
		for _, u := range []struct {
			id   string
			role auth.Role
		}{{"u-viewer", auth.RoleViewer}, {"u-owner", auth.RoleMember}, {"u-outsider", auth.RoleMember}, {"u-admin", auth.RoleAdmin}} {
			seedUserWithRole(t, rt, u.id, u.id+"@acme.io", u.role)
		}
		orgID := seedOrg(t, rt, "Acme", "u-owner")
		addOrgMember(t, rt, orgID, "u-viewer", auth.RoleViewer)
		// In the store as on Postgres: the CI credentials' foreign key needs the organisation there.
		mirrorOrg(t, rt, orgID)
		seedFlow("f-mine", "u-owner")
		seedOrgFlow(t, rt, "f-org", "u-owner", orgID)
		fill := strings.NewReplacer("{org}", orgID, "{mine}", "f-mine", "{orgflow}", "f-org")

		bearer := ""
		if id.user != "" {
			pair, err := rt.security.AuthMgr.Issue(id.user, id.user+"@acme.io", id.role)
			if err != nil {
				t.Fatalf("%s: issue session: %v", id.name, err)
			}
			bearer = "Bearer " + pair.AccessToken
			if id.scopes != nil {
				bearer = mintPAT(t, rt, bearer, id.scopes)
			}
		}
		for _, ep := range endpoints {
			var body any
			if ep.body != nil {
				raw, _ := json.Marshal(ep.body)
				if err := json.Unmarshal([]byte(fill.Replace(string(raw))), &body); err != nil {
					t.Fatal(err)
				}
			}
			rr := doRequestWithAuth(t, rt, ep.method, fill.Replace(ep.path), bearer, body)
			fmt.Fprintf(&b, "%-15s %-6s %-40s %d\n", id.name, ep.method, ep.path, rr.Code)
		}
	}
	compareGolden(t, filepath.Join("testdata", "authz_matrix.golden"), b.String(), *updateAuthzMatrix, "-update-authz-matrix")
}

// mintPAT creates a scoped API token through the API as the session in bearer.
func mintPAT(t *testing.T, rt *Router, bearer string, scopes []string) string {
	t.Helper()
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/tokens", bearer, map[string]any{"name": "matrix", "scopes": scopes})
	checkStatus(t, rr, http.StatusOK)
	var created struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil || created.Token == "" {
		t.Fatalf("mint PAT: %v (%s)", err, rr.Body.String())
	}
	return "Bearer " + created.Token
}
