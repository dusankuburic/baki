package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/testutil"
)

// sessionBearer is an interactive session for userID with the global role.
func sessionBearer(t *testing.T, rt *Router, userID string, role auth.Role) string {
	t.Helper()
	pair, err := rt.security.AuthMgr.Issue(userID, userID+"@example.com", role)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	return "Bearer " + pair.AccessToken
}

// adminRoutePolicies lists the registered /api/admin routes, AdminHandler's and SystemHandler's.
func adminRoutePolicies(t *testing.T, rt *Router) []*route.Policy {
	t.Helper()
	var out []*route.Policy
	for _, p := range rt.routes.Policies() {
		if strings.HasPrefix(p.Pattern, "/api/admin/") {
			out = append(out, p)
		}
	}
	if len(out) < 9 {
		t.Fatalf("found %d /api/admin routes, want at least 9", len(out))
	}
	return out
}

// Every /api/admin route sits behind the one group middleware (requireInstance on service.InstanceAdmin): a member's
// session is refused with 403 "forbidden" before any handler runs, an admin's reaches the handler.
func TestAdminRoutes_InstanceAdminGroup(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "u-admin", "u-admin@example.com", auth.RoleAdmin)
	seedUserWithRole(t, rt, "u-member", "u-member@example.com", auth.RoleMember)
	admin, member := sessionBearer(t, rt, "u-admin", auth.RoleAdmin), sessionBearer(t, rt, "u-member", auth.RoleMember)

	for _, p := range adminRoutePolicies(t, rt) {
		path := route.SamplePath(p.Pattern)
		rr := doRequestWithAuth(t, rt, p.Method, path, member, nil)
		if rr.Code != http.StatusForbidden || errorEnvelope(t, rr).Message != "forbidden" {
			t.Errorf("member %s %s = %d %s, want 403 forbidden", p.Method, path, rr.Code, rr.Body.String())
		}
		if rr := doRequestWithAuth(t, rt, p.Method, path, admin, nil); rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
			t.Errorf("admin %s %s = %d %s, want the handler's answer", p.Method, path, rr.Code, rr.Body.String())
		}
	}
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list", admin, nil), http.StatusOK)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/system/health", admin, nil), http.StatusOK)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list", "", nil), http.StatusUnauthorized)
}

// Without accounts every caller is the local user, whom the admin group lets through as RequireRole did.
func TestAdminRoutes_LocalModePasses(t *testing.T) {
	store := testutil.NewFakeBackend()
	rt := newTestRouter(store, false)
	for _, p := range adminRoutePolicies(t, rt) {
		path := route.SamplePath(p.Pattern)
		if rr := doRequest(t, rt, p.Method, path, nil); rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
			t.Errorf("local %s %s = %d %s, want the handler's answer", p.Method, path, rr.Code, rr.Body.String())
		}
	}
	checkStatus(t, doRequest(t, rt, http.MethodGet, "/api/admin/users/list", nil), http.StatusOK)
}

// A scoped API token without the admin scope is refused at the route, before the group middleware; with the scope,
// the middleware still decides on its owner's global role.
func TestAdminRoutes_ScopedTokens(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "u-admin", "u-admin@example.com", auth.RoleAdmin)
	seedUserWithRole(t, rt, "u-member", "u-member@example.com", auth.RoleMember)
	admin, member := sessionBearer(t, rt, "u-admin", auth.RoleAdmin), sessionBearer(t, rt, "u-member", auth.RoleMember)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list", mintPAT(t, rt, admin, []string{auth.ScopeRead, auth.ScopeWrite}), nil)
	if rr.Code != http.StatusForbidden || !strings.Contains(errorEnvelope(t, rr).Message, "missing scope") {
		t.Errorf("admin's read+write token = %d %s, want the route's 403 for the missing scope", rr.Code, rr.Body.String())
	}
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list", mintPAT(t, rt, admin, []string{auth.ScopeAdmin}), nil), http.StatusOK)
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list", mintPAT(t, rt, member, []string{auth.ScopeAdmin}), nil)
	if rr.Code != http.StatusForbidden || errorEnvelope(t, rr).Message != "forbidden" {
		t.Errorf("member's admin-scoped token = %d %s, want the group's 403 forbidden", rr.Code, rr.Body.String())
	}
}

// The org routes' checks, on the route (requireOrg) and in the handlers (authorizeOrg, canOrg), answer an outsider,
// a viewer, a member and an admin of the organisation as before: 403 for a refusal, the handler's own answer
// otherwise; and an unknown organisation is 403 for the member actions, 404 for the admin ones.
func TestOrgRoutes_RoleChecks(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "u-admin")
	addOrgMember(t, rt, orgID, "u-member", auth.RoleMember)
	addOrgMember(t, rt, orgID, "u-viewer", auth.RoleViewer)
	org := "/api/orgs/" + orgID
	do := func(user, method, path string, body any) int {
		t.Helper()
		return doRequestWithAuth(t, rt, method, path, sessionBearer(t, rt, user, auth.RoleMember), body).Code
	}

	cases := []struct {
		name, method, path              string
		body                            any
		outsider, viewer, member, admin int
	}{
		{"rule list (route, member)", http.MethodGet, org + "/rules", nil, 403, 200, 200, 200},
		{"rule export (route, member)", http.MethodGet, org + "/rules/export", nil, 403, 200, 200, 200},
		{"rule save (route, admin)", http.MethodPost, org + "/rules", map[string]any{}, 403, 403, 403, 400},
		{"channel save (route, admin)", http.MethodPost, org + "/channels", map[string]any{"kind": "pigeon"}, 403, 403, 403, 400},
		{"channel list (handler, member)", http.MethodGet, org + "/channels", nil, 403, 200, 200, 200},
		{"member list (handler, member)", http.MethodGet, org + "/members", nil, 403, 200, 200, 200},
		{"rename (handler, admin)", http.MethodPut, org, map[string]any{"name": " "}, 403, 403, 403, 400},
		{"role change (handler, admin)", http.MethodPut, org + "/members/u-member/role", map[string]any{"role": "owner"}, 403, 403, 403, 400},
		{"invite revoke (handler, admin)", http.MethodDelete, org + "/invites/no-such-invite", nil, 403, 403, 403, 404},
	}
	for _, c := range cases {
		for _, who := range []struct {
			user string
			want int
		}{{"u-outsider", c.outsider}, {"u-viewer", c.viewer}, {"u-member", c.member}, {"u-admin", c.admin}} {
			if got := do(who.user, c.method, c.path, c.body); got != who.want {
				t.Errorf("%s as %s: status %d, want %d", c.name, who.user, got, who.want)
			}
		}
	}

	unknown := "/api/orgs/no-such-org"
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{http.MethodGet, unknown + "/rules", nil, 403},
		{http.MethodGet, unknown + "/members", nil, 403},
		{http.MethodPost, unknown + "/rules", map[string]any{}, 404},
		{http.MethodPut, unknown, map[string]any{"name": "x"}, 404},
		{http.MethodDelete, unknown + "/members/u-admin", nil, 404},
	} {
		if got := do("u-admin", c.method, c.path, c.body); got != c.want {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, got, c.want)
		}
	}

	// A member may leave; removing anyone else takes an admin.
	if got := do("u-member", http.MethodDelete, org+"/members/u-viewer", nil); got != http.StatusForbidden {
		t.Errorf("member removing a viewer: status %d, want 403", got)
	}
	if got := do("u-member", http.MethodDelete, org+"/members/u-member", nil); got != http.StatusOK {
		t.Errorf("member leaving: status %d, want 200", got)
	}
	if got := do("u-admin", http.MethodDelete, org+"/members/u-viewer", nil); got != http.StatusOK {
		t.Errorf("admin removing a viewer: status %d, want 200", got)
	}
}

// A scoped API token reaches the org routes its scopes allow, and the org checks decide on its owner's role. The
// invite list stays on the role check, which demands no write scope from a read-only token.
func TestOrgRoutes_ScopedTokens(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "u-admin", "u-admin@example.com", auth.RoleMember)
	orgID := seedOrg(t, rt, "Acme", "u-admin")
	org := "/api/orgs/" + orgID
	session := sessionBearer(t, rt, "u-admin", auth.RoleMember)
	read := mintPAT(t, rt, session, []string{auth.ScopeRead})
	readWrite := mintPAT(t, rt, session, []string{auth.ScopeRead, auth.ScopeWrite})

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, org+"/rules", read, nil), http.StatusOK)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, org+"/members", read, nil), http.StatusOK)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, org+"/invites", read, nil), http.StatusOK)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, org+"/rules", read, map[string]any{}), http.StatusForbidden)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, org+"/rules", readWrite, map[string]any{}), http.StatusBadRequest)
}

// serverStoreRoutes are the routes whose handlers answered 503 without a storage backend and now declare the server
// store instead (route.Needs).
var serverStoreRoutes = []struct{ method, pattern string }{
	{http.MethodGet, "/api/admin/users/list"},
	{http.MethodGet, "/api/admin/users/stats"},
	{http.MethodPut, "/api/admin/users/{id}/role"},
	{http.MethodGet, "/api/admin/audit"},
	{http.MethodGet, "/api/auth/tokens"},
	{http.MethodPost, "/api/auth/tokens"},
	{http.MethodDelete, "/api/auth/tokens/{id}"},
	{http.MethodDelete, "/api/auth/account"},
	{http.MethodGet, "/api/auth/account/export"},
	{http.MethodGet, "/api/orgs/{id}/rules"},
	{http.MethodGet, "/api/orgs/{id}/rules/export"},
	{http.MethodPost, "/api/orgs/{id}/rules"},
	{http.MethodDelete, "/api/orgs/{id}/rules/{ruleId}"},
	{http.MethodPost, "/api/orgs/{id}/channels"},
	{http.MethodPost, "/api/orgs/{id}/channels/{channelId}/test"},
	{http.MethodDelete, "/api/orgs/{id}/channels/{channelId}"},
	{http.MethodGet, "/api/orgs/{id}/knowledge"},
	{http.MethodPost, "/api/orgs/{id}/knowledge/upload"},
	{http.MethodPost, "/api/orgs/{id}/knowledge/reindex"},
	{http.MethodDelete, "/api/orgs/{id}/knowledge/{docId}"},
}

// The routes that need the server store declare it: on the desktop, which has none, each answers 503
// SERVICE_UNAVAILABLE before its handler runs; with the store the guard lets them through.
func TestNeedsGating_ServerStoreRoutes(t *testing.T) {
	desktop := newTestRouter(nil, false)
	// Every deployment wires a knowledge service: the knowledge routes' 503 must be the route's, not the handlers'
	// "not configured".
	moduleOf[*OrgHandler](t, desktop).knowledge = rag.NewKnowledgeService(nil, nil, nil)
	for _, g := range serverStoreRoutes {
		if p := desktop.routes.Lookup(g.method, g.pattern); p == nil || !slices.Contains(p.Needs, capability.ServerStore) {
			t.Errorf("%s %s does not declare the server store", g.method, g.pattern)
			continue
		}
		path := route.SamplePath(g.pattern)
		rr := doRequest(t, desktop, g.method, path, nil)
		if rr.Code != http.StatusServiceUnavailable || errorEnvelope(t, rr).Code != "SERVICE_UNAVAILABLE" {
			t.Errorf("desktop %s %s = %d %s, want 503 SERVICE_UNAVAILABLE", g.method, path, rr.Code, rr.Body.String())
		}
	}

	cloud := newJWTTestRouter(t)
	bearer := jwtBearer(t, cloud, "u-1", "u1@example.com")
	checkStatus(t, doRequestWithAuth(t, cloud, http.MethodGet, "/api/auth/tokens", bearer, nil), http.StatusOK)
	checkStatus(t, doRequestWithAuth(t, cloud, http.MethodGet, "/api/admin/audit", bearer, nil), http.StatusOK)
}
