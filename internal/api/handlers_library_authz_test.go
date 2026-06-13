package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
)

// --- Unified library authorization (org roles + collaborator tiers) ---

// Org members must be able to read flows shared into their org — this was the
// headline gap of the stubbed LibraryService.CanRead.
func TestLibraryGet_OrgMemberCanRead(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	addOrgMember(t, rt, orgID, "bob", auth.RoleMember)
	seedOrgFlow(t, rt, "org-flow", "alice", orgID)
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/org-flow", bearer, nil)
	checkStatus(t, rr, http.StatusOK)

	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/library/org-flow/content", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
}

func TestLibraryUpdate_EditorCollaboratorAllowed(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	seedCollaborator(t, rt, "flow1", "dave", "editor")
	bearer := jwtBearer(t, rt, "dave", "dave@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/flow1", bearer, map[string]any{
		"name": "edited by collaborator",
	})
	checkStatus(t, rr, http.StatusOK)
}

func TestLibraryUpdate_ViewerCollaboratorForbidden(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	seedCollaborator(t, rt, "flow1", "erin", "viewer")
	bearer := jwtBearer(t, rt, "erin", "erin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/flow1", bearer, map[string]any{
		"name": "should not work",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestLibraryUpdate_OrgMemberAllowed(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	addOrgMember(t, rt, orgID, "bob", auth.RoleMember)
	seedOrgFlow(t, rt, "org-flow", "alice", orgID)
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/org-flow", bearer, map[string]any{
		"name": "edited by org member",
	})
	checkStatus(t, rr, http.StatusOK)
}

func TestLibraryUpdate_OrgViewerForbidden(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	addOrgMember(t, rt, orgID, "carol", auth.RoleViewer)
	seedOrgFlow(t, rt, "org-flow", "alice", orgID)
	bearer := jwtBearer(t, rt, "carol", "carol@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/org-flow", bearer, map[string]any{
		"name": "should not work",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestLibraryDelete_OrgAdminAllowed(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice") // alice is org admin
	addOrgMember(t, rt, orgID, "bob", auth.RoleMember)
	seedOrgFlow(t, rt, "org-flow", "bob", orgID) // bob owns the flow
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/library/org-flow", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
}

func TestLibraryDelete_OrgMemberForbidden(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	addOrgMember(t, rt, orgID, "bob", auth.RoleMember)
	seedOrgFlow(t, rt, "org-flow", "alice", orgID)
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/library/org-flow", bearer, nil)
	checkStatus(t, rr, http.StatusForbidden)
}

// A collaborator with the admin tier manages sharing but must not delete.
func TestLibraryDelete_CollabAdminForbidden(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	seedCollaborator(t, rt, "flow1", "dave", "admin")
	bearer := jwtBearer(t, rt, "dave", "dave@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/library/flow1", bearer, nil)
	checkStatus(t, rr, http.StatusForbidden)
}

// --- Sharing management with the "admin" rank ---

func TestCollaboratorAdd_CollabAdminAllowed(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow1", "alice")
	seedCollaborator(t, rt, "flow1", "dave", "admin")
	seedUser(t, rt, "u1", "new@example.com")
	bearer := jwtBearer(t, rt, "dave", "dave@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow1/collaborators", bearer, map[string]any{
		"email": "new@example.com", "permission": "viewer",
	})
	checkStatus(t, rr, http.StatusOK)
}

func TestCollaboratorAdd_EditorForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow1", "alice")
	seedCollaborator(t, rt, "flow1", "dave", "editor")
	seedUser(t, rt, "u1", "new@example.com")
	bearer := jwtBearer(t, rt, "dave", "dave@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow1/collaborators", bearer, map[string]any{
		"email": "new@example.com", "permission": "viewer",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestCollaboratorAdd_OrgAdminAllowed(t *testing.T) {
	rt := newJWTTestRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice") // alice is org admin
	addOrgMember(t, rt, orgID, "bob", auth.RoleMember)
	seedOrgFlow(t, rt, "org-flow", "bob", orgID)
	seedUser(t, rt, "u1", "new@example.com")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/org-flow/collaborators", bearer, map[string]any{
		"email": "new@example.com", "permission": "viewer",
	})
	checkStatus(t, rr, http.StatusOK)
}

// --- Version creation authz ---

func TestSaveVersion_ViewerCollaboratorForbidden(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	seedCollaborator(t, rt, "flow1", "erin", "viewer")
	bearer := jwtBearer(t, rt, "erin", "erin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/library/flow1/versions", bearer, map[string]any{
		"comment": "should not work",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestSaveVersion_EditorCollaboratorAllowed(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	seedCollaborator(t, rt, "flow1", "dave", "editor")
	bearer := jwtBearer(t, rt, "dave", "dave@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/library/flow1/versions", bearer, map[string]any{
		"comment": "v2",
	})
	checkStatus(t, rr, http.StatusCreated)
}
