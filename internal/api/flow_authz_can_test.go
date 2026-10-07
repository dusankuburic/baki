package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/service"
)

// A chat-only API token on a route whose scope admits it, but whose handler needs FlowEdit, is refused by Can (403,
// the scope refusal) before anything is resolved: the route scope no longer has to imply what the handler does
// (S2). The probes sit under /api/chat with the chat scope, as the chat routes do, so the legacy scope table and the
// declared policy both let the token through to the handler; the same token may use the flow for ChatUse, and a
// token holding write may edit it.
func TestCan_ChatTokenRefusedOnAFlowEditRoute(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seedUserWithRole(t, rt, "u-owner", "u-owner@acme.io", auth.RoleMember)
	seed("f-chat", "u-owner")
	flows := moduleOf[*FlowHandler](t, rt).flowSvc
	probe := func(action service.Action) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if requireFlowPerm(w, r, flows, rt.security, r.URL.Query().Get("flowId"), action) {
				w.WriteHeader(http.StatusNoContent)
			}
		}
	}
	mountTestRoutes(t, rt, func(rb *route.Builder) {
		rb.Post("/api/chat/f1-probe/edit", probe(service.FlowEdit), route.Scopes(auth.ScopeChat), route.NoTx())
		rb.Post("/api/chat/f1-probe/chat", probe(service.ChatUse), route.Scopes(auth.ScopeChat), route.NoTx())
	})
	session := jwtBearer(t, rt, "u-owner", "u-owner@acme.io")
	chatOnly := mintPAT(t, rt, session, []string{auth.ScopeChat})
	chatWrite := mintPAT(t, rt, session, []string{auth.ScopeChat, auth.ScopeWrite})

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/f1-probe/edit?flowId=f-chat", chatOnly, nil)
	checkStatus(t, rr, http.StatusForbidden)
	if body := rr.Body.String(); !strings.Contains(body, "missing API token scope") || !strings.Contains(body, "flow.edit") {
		t.Errorf("chat-only token on FlowEdit: body %s, want Can's scope refusal", body)
	}
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/f1-probe/chat?flowId=f-chat", chatOnly, nil), http.StatusNoContent)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/f1-probe/edit?flowId=f-chat", chatWrite, nil), http.StatusNoContent)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/f1-probe/edit?flowId=f-chat", session, nil), http.StatusNoContent)
}

// The same refusal on a real FlowEdit handler (save-source) when nothing in front of it checked the token's
// scopes: the handler's own flow check is Can's.
func TestCan_ChatTokenRefusedBySaveSource(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedCloudFlowWithSource(t, rt, "f-src", "u-owner", "#Region \"Main\"\nWAIT 1\n#EndRegion\n")
	h := moduleOf[*FlowHandler](t, rt)
	save := func(claims *auth.Claims) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"flowId": "f-src", "source": "#Region \"Main\"\nWAIT 2\n#EndRegion\n"})
		req := httptest.NewRequest(http.MethodPost, "/api/flow/save-source", bytes.NewReader(body))
		req = req.WithContext(auth.WithClaims(req.Context(), claims))
		rr := httptest.NewRecorder()
		h.handleSaveSource(rr, req)
		return rr
	}

	rr := save(auth.ClaimsForPAT("u-owner", "u-owner@acme.io", auth.RoleMember, "tok-chat", nil, []string{auth.ScopeChat}))
	checkStatus(t, rr, http.StatusForbidden)
	if !strings.Contains(rr.Body.String(), "missing API token scope") {
		t.Errorf("chat-only token: body %s, want Can's scope refusal", rr.Body.String())
	}
	checkStatus(t, save(auth.ClaimsForPAT("u-owner", "u-owner@acme.io", auth.RoleMember, "tok-write", nil, []string{auth.ScopeWrite})), http.StatusOK)
}

// The collaborator routes authorize the flow in their path before the handler runs (requireFlow): reading the
// list needs FlowRead, adding or changing a collaborator FlowShare. A removal decides in the handler, because
// anyone may remove themselves. The statuses are the ones the in-handler checks answered: 404 for a missing
// flow, 403 for an outsider, and the refusal comes before the body is read.
func TestCollaboratorRoutes_AuthorizeTheFlowInThePath(t *testing.T) {
	type call struct {
		method, path string
		body         any
	}
	list := call{http.MethodGet, "/api/flows/{flow}/collaborators", nil}
	add := call{http.MethodPost, "/api/flows/{flow}/collaborators", map[string]any{"userId": "u-outsider", "permission": "viewer"}}
	update := call{http.MethodPut, "/api/flows/{flow}/collaborators/u-collab", map[string]any{"permission": "editor"}}
	remove := call{http.MethodDelete, "/api/flows/{flow}/collaborators/u-collab", nil}
	badBody := call{http.MethodPost, "/api/flows/{flow}/collaborators", "not an object"}

	cases := []struct {
		name   string
		caller string
		flow   string
		call   call
		want   int
	}{
		{"owner lists", "u-owner", "f-org", list, http.StatusOK},
		{"owner adds", "u-owner", "f-org", add, http.StatusOK},
		{"owner updates", "u-owner", "f-org", update, http.StatusOK},
		{"owner removes", "u-owner", "f-org", remove, http.StatusOK},
		{"owner sends a bad body", "u-owner", "f-org", badBody, http.StatusBadRequest},
		{"org viewer lists", "u-viewer", "f-org", list, http.StatusOK},
		{"org viewer adds", "u-viewer", "f-org", add, http.StatusForbidden},
		{"org viewer updates", "u-viewer", "f-org", update, http.StatusForbidden},
		{"org viewer removes another", "u-viewer", "f-org", remove, http.StatusForbidden},
		{"collaborator removes themselves", "u-collab", "f-org", remove, http.StatusOK},
		{"outsider lists", "u-outsider", "f-org", list, http.StatusForbidden},
		{"outsider adds", "u-outsider", "f-org", add, http.StatusForbidden},
		{"outsider updates", "u-outsider", "f-org", update, http.StatusForbidden},
		{"outsider removes another", "u-outsider", "f-org", remove, http.StatusForbidden},
		{"outsider sends a bad body", "u-outsider", "f-org", badBody, http.StatusForbidden},
		{"missing flow: list", "u-owner", "f-ghost", list, http.StatusNotFound},
		{"missing flow: add", "u-owner", "f-ghost", add, http.StatusNotFound},
		{"missing flow: update", "u-owner", "f-ghost", update, http.StatusNotFound},
		{"missing flow: remove another", "u-owner", "f-ghost", remove, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, _ := newLibraryTestRouter(t)
			for _, u := range []string{"u-owner", "u-viewer", "u-outsider", "u-collab"} {
				seedUserWithRole(t, rt, u, u+"@acme.io", auth.RoleMember)
			}
			orgID := seedOrg(t, rt, "Acme", "u-owner")
			addOrgMember(t, rt, orgID, "u-viewer", auth.RoleViewer)
			seedOrgFlow(t, rt, "f-org", "u-owner", orgID)
			seedCollaborator(t, rt, "f-org", "u-collab", "viewer")

			path := strings.ReplaceAll(tc.call.path, "{flow}", tc.flow)
			rr := doRequestWithAuth(t, rt, tc.call.method, path, jwtBearer(t, rt, tc.caller, tc.caller+"@acme.io"), tc.call.body)
			checkStatus(t, rr, tc.want)
		})
	}
}

// storeRoutes are the routes of this package's flow-centric handlers that need the server-side store. They used
// to answer 503 from the handler; the route now declares route.Needs(capability.ServerStore).
var storeRoutes = []struct{ method, pattern string }{
	{http.MethodPost, "/api/flow/search-library"},
	{http.MethodPut, "/api/flow/tags"},
	{http.MethodPost, "/api/flow/share/create"},
	{http.MethodPost, "/api/flow/share/revoke"},
	{http.MethodGet, "/api/shared"},
	{http.MethodPost, "/api/analysis/comments/add"},
	{http.MethodPost, "/api/analysis/comments/delete"},
	{http.MethodPost, "/api/analysis/policy/save"},
	{http.MethodPost, "/api/analysis/policy/list"},
	{http.MethodPost, "/api/analysis/policy/get"},
	{http.MethodPost, "/api/analysis/policy/delete"},
	{http.MethodPost, "/api/analysis/policy/evaluate-by-id"},
	{http.MethodPost, "/api/analysis/triage/list"},
	{http.MethodPost, "/api/analysis/triage/set"},
	{http.MethodPost, "/api/analysis/triage/set-batch"},
	{http.MethodPost, "/api/analysis/triage/clear"},
	{http.MethodPost, "/api/analysis/baseline/get"},
	{http.MethodPost, "/api/analysis/baseline/set"},
	{http.MethodPost, "/api/analysis/baseline/clear"},
	{http.MethodPost, "/api/analysis/baseline/drift"},
	{http.MethodGet, "/api/governance/alerts"},
	{http.MethodGet, "/api/governance/alerts/unread-count"},
	{http.MethodPost, "/api/governance/alerts/read-all"},
	{http.MethodPost, "/api/governance/alerts/dismiss"},
	{http.MethodDelete, "/api/governance/alerts"},
	{http.MethodPost, "/api/flows/{flowId}/collaborators"},
	{http.MethodPut, "/api/flows/{flowId}/collaborators/{userId}"},
	{http.MethodDelete, "/api/flows/{flowId}/collaborators/{userId}"},
	{http.MethodPost, "/api/library/{id}/versions"},
	{http.MethodGet, "/api/library/{id}/versions/{vn}"},
	{http.MethodPost, "/api/library/{id}/versions/{vn}/restore"},
	{http.MethodGet, "/api/library/{id}/versions/{vn}/diff"},
}

// On the desktop (no server-side store) every store route answers 503 SERVICE_UNAVAILABLE from the route guard,
// before its handler could reach the missing backend, whatever the body; the routes that list (collaborators,
// versions, comments, share links) keep answering an empty list there.
func TestStoreRoutes_DesktopAnswersServiceUnavailable(t *testing.T) {
	rt := newTestRouter(nil, false)
	for _, sr := range storeRoutes {
		p := rt.routes.Lookup(sr.method, sr.pattern)
		if p == nil || !slices.Contains(p.Needs, capability.ServerStore) {
			t.Errorf("%s %s: policy %+v does not declare Needs(ServerStore)", sr.method, sr.pattern, p)
			continue
		}
		for _, body := range []any{nil, map[string]any{"flowId": "f1", "orgId": "o1", "findingKey": "k", "status": "open", "id": "x"}} {
			rr := doRequest(t, rt, sr.method, route.SamplePath(sr.pattern), body)
			if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"code":"SERVICE_UNAVAILABLE"`) {
				t.Errorf("desktop %s %s = %d %s, want 503 SERVICE_UNAVAILABLE", sr.method, sr.pattern, rr.Code, rr.Body.String())
			}
		}
	}
	for _, list := range []struct{ method, path string }{
		{http.MethodGet, "/api/flows/f1/collaborators"},
		{http.MethodGet, "/api/library/f1/versions"},
		{http.MethodPost, "/api/analysis/comments/list"},
		{http.MethodPost, "/api/flow/share/list"},
	} {
		rr := doRequest(t, rt, list.method, list.path, map[string]any{"flowId": "f1", "findingKey": "k"})
		if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "[]" {
			t.Errorf("desktop %s %s = %d %s, want 200 []", list.method, list.path, rr.Code, rr.Body.String())
		}
	}
}

// With a store the guard lets the store routes through to their handlers: none answers the guard's 503.
func TestStoreRoutes_ServedWithAStore(t *testing.T) {
	rt := newJWTTestRouter(t)
	bearer := jwtBearer(t, rt, "u-1", "u1@example.com")
	for _, sr := range storeRoutes {
		rr := doRequestWithAuth(t, rt, sr.method, route.SamplePath(sr.pattern), bearer, map[string]any{"flowId": "f-none", "orgId": "o-none"})
		if rr.Code == http.StatusServiceUnavailable {
			t.Errorf("%s %s = 503 %s with a store", sr.method, sr.pattern, rr.Body.String())
		}
	}
}

// requireOrgMember and requireOrgAdmin decide through Can: the org role, and an API token's scope for the action
// (OrgRead takes read or write, OrgAdmin write). Every refusal keeps the 403 and the text they always answered.
func TestRequireOrgHelpers_DecideThroughCan(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	orgID := seedOrg(t, rt, "Acme", "u-admin")
	addOrgMember(t, rt, orgID, "u-viewer", auth.RoleViewer)
	run := func(check func(http.ResponseWriter, *http.Request, *SecurityConfig, string) bool, claims *auth.Claims, org string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(auth.WithClaims(req.Context(), claims))
		rr := httptest.NewRecorder()
		if check(rr, req, rt.security, org) {
			rr.WriteHeader(http.StatusNoContent)
		}
		return rr
	}
	session := func(user string) *auth.Claims { return &auth.Claims{UserID: user, Role: auth.RoleMember} }
	token := func(user string, scopes ...string) *auth.Claims {
		return auth.ClaimsForPAT(user, user+"@acme.io", auth.RoleMember, "tok", nil, scopes)
	}
	const notMember, notAdmin = "forbidden: not a member of this organization", "forbidden: organization admin required"
	cases := []struct {
		name  string
		check func(http.ResponseWriter, *http.Request, *SecurityConfig, string) bool
		who   *auth.Claims
		org   string
		want  int
		body  string
	}{
		{"member reads", requireOrgMember, session("u-viewer"), orgID, http.StatusNoContent, ""},
		{"outsider reads", requireOrgMember, session("u-out"), orgID, http.StatusForbidden, notMember},
		{"unknown organisation", requireOrgMember, session("u-admin"), "no-such-org", http.StatusForbidden, notMember},
		{"read token reads", requireOrgMember, token("u-viewer", auth.ScopeRead), orgID, http.StatusNoContent, ""},
		{"chat token reads", requireOrgMember, token("u-viewer", auth.ScopeChat), orgID, http.StatusForbidden, notMember},
		{"admin administers", requireOrgAdmin, session("u-admin"), orgID, http.StatusNoContent, ""},
		{"viewer administers", requireOrgAdmin, session("u-viewer"), orgID, http.StatusForbidden, notAdmin},
		{"unknown organisation administered", requireOrgAdmin, session("u-admin"), "no-such-org", http.StatusForbidden, notAdmin},
		{"write token administers", requireOrgAdmin, token("u-admin", auth.ScopeWrite), orgID, http.StatusNoContent, ""},
		{"read token administers", requireOrgAdmin, token("u-admin", auth.ScopeRead), orgID, http.StatusForbidden, notAdmin},
	}
	for _, tc := range cases {
		rr := run(tc.check, tc.who, tc.org)
		if rr.Code != tc.want || !strings.Contains(rr.Body.String(), tc.body) {
			t.Errorf("%s: %d %s, want %d %q", tc.name, rr.Code, rr.Body.String(), tc.want, tc.body)
		}
	}
}

// requireInstancePerm is RequireRole decided by Can: InstanceWrite admits the admin and member roles and a token
// holding write; a viewer, a guest and a read-only token get RequireRole's 403. Local mode passes.
func TestRequireInstance_InstanceWrite(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	run := func(security *SecurityConfig, claims *auth.Claims) int {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if claims != nil {
			req = req.WithContext(auth.WithClaims(req.Context(), claims))
		}
		rr := httptest.NewRecorder()
		if requireInstancePerm(rr, req, security, service.InstanceWrite) {
			return http.StatusNoContent
		}
		if rr.Code == http.StatusForbidden && !strings.Contains(rr.Body.String(), `"message":"forbidden"`) {
			t.Errorf("refusal body %s, want RequireRole's forbidden", rr.Body.String())
		}
		return rr.Code
	}
	pat := func(role auth.Role, scopes ...string) *auth.Claims {
		return auth.ClaimsForPAT("u-1", "u1@acme.io", role, "tok", nil, scopes)
	}
	cases := []struct {
		name   string
		claims *auth.Claims
		want   int
	}{
		{"admin", &auth.Claims{UserID: "u-1", Role: auth.RoleAdmin}, http.StatusNoContent},
		{"member", &auth.Claims{UserID: "u-1", Role: auth.RoleMember}, http.StatusNoContent},
		{"viewer", &auth.Claims{UserID: "u-1", Role: auth.RoleViewer}, http.StatusForbidden},
		{"guest", &auth.Claims{UserID: "u-1", Role: auth.RoleGuest}, http.StatusForbidden},
		{"member's write token", pat(auth.RoleMember, auth.ScopeWrite), http.StatusNoContent},
		{"member's read token", pat(auth.RoleMember, auth.ScopeRead), http.StatusForbidden},
		{"viewer's write token", pat(auth.RoleViewer, auth.ScopeWrite), http.StatusForbidden},
	}
	for _, tc := range cases {
		if got := run(rt.security, tc.claims); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := run(newTestRouter(nil, false).security, nil); got != http.StatusNoContent {
		t.Errorf("local mode: %d, want it to pass as RequireRole does", got)
	}
}
