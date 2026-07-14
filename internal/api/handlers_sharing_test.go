package api

import (
	"context"
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

func seedUser(t *testing.T, rt *Router, id, email string) {
	t.Helper()
	err := rt.security.Backend.SaveUser(context.Background(), &storageif.User{
		ID:    id,
		Email: email,
		Role:  auth.RoleMember,
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

// seedFlow inserts a flow owned by ownerID so sharing handlers' ownership check passes.
func seedFlow(t *testing.T, rt *Router, flowID, ownerID string) {
	t.Helper()
	err := rt.security.Backend.SaveFlow(context.Background(), &storageif.FlowDocument{
		ID:      flowID,
		Name:    "test",
		OwnerID: ownerID,
	})
	if err != nil {
		t.Fatalf("seed flow: %v", err)
	}
}

func TestHandleSharingList_EmptyByDefault(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/flows/flow-1/collaborators", token, nil)
	checkStatus(t, rr, http.StatusOK)
	var collabs []any
	decodeJSON(t, rr, &collabs)
	if len(collabs) != 0 {
		t.Errorf("expected empty list, got %d", len(collabs))
	}
}

func TestHandleSharingAdd_OK(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")
	seedUser(t, rt, "u1", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"email": "alice@example.com", "permission": "viewer",
	})
	checkStatus(t, rr, http.StatusOK)
	var c map[string]any
	decodeJSON(t, rr, &c)
	if c["email"] != "alice@example.com" {
		t.Errorf("expected email alice@example.com, got %v", c["email"])
	}
	if c["permission"] != "viewer" {
		t.Errorf("expected permission viewer, got %v", c["permission"])
	}
}

func TestHandleSharingAdd_NonOwnerReturns403(t *testing.T) {
	rt := newJWTTestRouter(t)
	// flow-1 is owned by "someone-else"; the caller is "admin".
	seedFlow(t, rt, "flow-1", "someone-else")
	seedUser(t, rt, "u1", "alice@example.com")
	token := jwtBearer(t, rt, "admin", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"email": "alice@example.com", "permission": "viewer",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestHandleSharingAdd_MissingFlowReturns404(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	// no flow seeded
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/ghost/collaborators", token, map[string]any{
		"email": "alice@example.com",
	})
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleSharingAdd_DefaultsToViewer(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")
	seedUser(t, rt, "u1", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"userId": "u1",
	})
	checkStatus(t, rr, http.StatusOK)
	var c map[string]any
	decodeJSON(t, rr, &c)
	if c["permission"] != "viewer" {
		t.Errorf("expected default permission viewer, got %v", c["permission"])
	}
}

func TestHandleSharingAdd_UnknownUserReturnsOk_NoEnumeration(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"email": "nobody@example.com",
	})
	// Anti-enumeration (M-Wave4): an unknown email returns the same 200 "ok" as
	// a successful add, so an admin can't probe whether an email is registered.
	// The collaborator is NOT actually added.
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleSharingAdd_InvalidPermissionReturns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")
	seedUser(t, rt, "u1", "eve@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"userId": "u1", "permission": "superuser",
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleSharingAdd_MissingInputReturns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"permission": "viewer",
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleSharingUpdate_OK(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")
	seedUser(t, rt, "u1", "alice@example.com")

	doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"userId": "u1", "permission": "viewer",
	})
	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/flows/flow-1/collaborators/u1", token, map[string]any{
		"permission": "editor",
	})
	checkStatus(t, rr, http.StatusOK)
	var c map[string]any
	decodeJSON(t, rr, &c)
	if c["permission"] != "editor" {
		t.Errorf("expected permission editor, got %v", c["permission"])
	}
}

func TestHandleSharingRemove_OK(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")
	seedUser(t, rt, "u1", "alice@example.com")

	doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"userId": "u1", "permission": "viewer",
	})
	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/flows/flow-1/collaborators/u1", token, nil)
	checkStatus(t, rr, http.StatusOK)
}

// TestHandleSharingRemove_SelfRemovalByCollaborator is the regression test for
// the S1 fix: a non-owner collaborator removing THEMSELVES must get a single
// clean 200. Before the fix, requireFlowOwner wrote a 403 to the response as a
// side effect, the self-removal branch then proceeded to delete the row, and
// render.JSON's second WriteHeader was dropped — so the client saw 403 while the
// removal actually happened. The recorder captures that first (403) write.
func TestHandleSharingRemove_SelfRemovalByCollaborator(t *testing.T) {
	rt := newJWTTestRouter(t)
	ownerToken := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")
	seedUser(t, rt, "u1", "alice@example.com")

	// Owner adds alice as a collaborator.
	addRR := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", ownerToken, map[string]any{
		"userId": "u1", "permission": "viewer",
	})
	checkStatus(t, addRR, http.StatusOK)

	// Alice (a non-owner) removes herself.
	aliceToken := jwtBearer(t, rt, "u1", "alice@example.com")
	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/flows/flow-1/collaborators/u1", aliceToken, nil)
	checkStatus(t, rr, http.StatusOK)

	// And she's actually gone.
	collabs, err := rt.security.Backend.ListCollaborators(context.Background(), "flow-1")
	if err != nil {
		t.Fatalf("ListCollaborators: %v", err)
	}
	if len(collabs) != 0 {
		t.Errorf("expected collaborator removed, still have %d", len(collabs))
	}
}

// TestHandleSharingRemove_OutsiderForbidden confirms a user who is neither the
// flow owner nor the target collaborator cannot remove anyone, and the target
// is left intact.
func TestHandleSharingRemove_OutsiderForbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	ownerToken := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")
	seedUser(t, rt, "u1", "alice@example.com")
	seedUser(t, rt, "u2", "eve@example.com")

	addRR := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", ownerToken, map[string]any{
		"userId": "u1", "permission": "viewer",
	})
	checkStatus(t, addRR, http.StatusOK)

	// Eve (neither owner nor target) tries to remove alice.
	eveToken := jwtBearer(t, rt, "u2", "eve@example.com")
	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/flows/flow-1/collaborators/u1", eveToken, nil)
	checkStatus(t, rr, http.StatusForbidden)

	// Alice is still a collaborator.
	collabs, err := rt.security.Backend.ListCollaborators(context.Background(), "flow-1")
	if err != nil {
		t.Fatalf("ListCollaborators: %v", err)
	}
	if len(collabs) != 1 {
		t.Errorf("expected alice to remain, have %d collaborators", len(collabs))
	}
}

func TestHandleSharingListAfterAdd(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	seedFlow(t, rt, "flow-1", "admin")
	seedUser(t, rt, "u1", "alice@example.com")

	doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"userId": "u1", "permission": "viewer",
	})
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/flows/flow-1/collaborators", token, nil)
	checkStatus(t, rr, http.StatusOK)
	var collabs []any
	decodeJSON(t, rr, &collabs)
	if len(collabs) != 1 {
		t.Errorf("expected 1 collaborator, got %d", len(collabs))
	}
}
