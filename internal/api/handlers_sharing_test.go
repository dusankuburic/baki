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
	err := rt.app.StorageBackend().SaveUser(context.Background(), &storageif.User{
		ID:    id,
		Email: email,
		Role:  auth.RoleMember,
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func TestHandleSharingList_EmptyByDefault(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	
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

func TestHandleSharingAdd_DefaultsToViewer(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
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

func TestHandleSharingAdd_UserNotFoundReturns404(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"email": "nobody@example.com",
	})
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleSharingAdd_InvalidPermissionReturns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	seedUser(t, rt, "u1", "eve@example.com")
	
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"userId": "u1", "permission": "superuser",
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleSharingAdd_MissingInputReturns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
	
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"permission": "viewer",
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleSharingUpdate_OK(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
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
	seedUser(t, rt, "u1", "alice@example.com")
	
	doRequestWithAuth(t, rt, http.MethodPost, "/api/flows/flow-1/collaborators", token, map[string]any{
		"userId": "u1", "permission": "viewer",
	})
	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/flows/flow-1/collaborators/u1", token, nil)
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleSharingListAfterAdd(t *testing.T) {
	rt := newJWTTestRouter(t)
	token := jwtBearer(t, rt, "admin", "admin@example.com")
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
