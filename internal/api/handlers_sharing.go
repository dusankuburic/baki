package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// handleSharingRoute dispatches /api/flows/:flowId/collaborators[/:userId]
func (rt *Router) handleSharingRoute(w http.ResponseWriter, r *http.Request) {
	// Path: /api/flows/<flowId>/collaborators  or
	//       /api/flows/<flowId>/collaborators/<userId>
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/flows/"), "/")
	// parts[0] = flowId, parts[1] = "collaborators", parts[2] (optional) = userId
	if len(parts) < 2 || parts[1] != "collaborators" {
		http.NotFound(w, r)
		return
	}
	flowID := parts[0]
	if flowID == "" {
		http.NotFound(w, r)
		return
	}

	if len(parts) == 2 {
		// /api/flows/:flowId/collaborators
		switch r.Method {
		case http.MethodGet:
			rt.handleListCollaborators(w, r, flowID)
		case http.MethodPost:
			rt.handleAddCollaborator(w, r, flowID)
		default:
			http.NotFound(w, r)
		}
		return
	}

	// /api/flows/:flowId/collaborators/:userId
	targetUserID := parts[2]
	if targetUserID == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		rt.handleUpdateCollaborator(w, r, flowID, targetUserID)
	case http.MethodDelete:
		rt.handleRemoveCollaborator(w, r, flowID, targetUserID)
	default:
		http.NotFound(w, r)
	}
}

func (rt *Router) handleListCollaborators(w http.ResponseWriter, r *http.Request, flowID string) {
	collabs, err := rt.app.StorageBackend().ListCollaborators(r.Context(), flowID)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, collabs)
}

func (rt *Router) handleAddCollaborator(w http.ResponseWriter, r *http.Request, flowID string) {
	if !rt.requireRole(w, r, auth.RoleMember) {
		return
	}

	var req struct {
		Email      string `json:"email"`
		UserID     string `json:"userId"`
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}

	var userID, email string

	if req.Email != "" {
		// Look up user by email
		u, err := rt.app.StorageBackend().LoadUserByEmail(r.Context(), req.Email)
		if err != nil {
			if errors.Is(err, storageif.ErrNotFound) {
				http.Error(w, "user not found", http.StatusNotFound)
				return
			}
			rt.sendError(w, err, http.StatusInternalServerError)
			return
		}
		userID = u.ID
		email = u.Email
	} else if req.UserID != "" {
		// Look up user by ID
		u, err := rt.app.StorageBackend().LoadUserByID(r.Context(), req.UserID)
		if err != nil {
			if errors.Is(err, storageif.ErrNotFound) {
				http.Error(w, "user not found", http.StatusNotFound)
				return
			}
			rt.sendError(w, err, http.StatusInternalServerError)
			return
		}
		userID = u.ID
		email = u.Email
	} else {
		http.Error(w, "email or userId is required", http.StatusBadRequest)
		return
	}

	if req.Permission == "" {
		req.Permission = "viewer"
	}
	if req.Permission != "viewer" && req.Permission != "editor" && req.Permission != "admin" {
		http.Error(w, "invalid permission", http.StatusBadRequest)
		return
	}

	c := &storageif.Collaborator{
		UserID:     userID,
		Email:      email,
		Permission: req.Permission,
		GrantedAt:  time.Now().UTC(),
	}
	if err := rt.app.StorageBackend().AddCollaborator(r.Context(), flowID, c); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, c)
}

func (rt *Router) handleUpdateCollaborator(w http.ResponseWriter, r *http.Request, flowID, userID string) {
	var req struct {
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if req.Permission != "viewer" && req.Permission != "editor" && req.Permission != "admin" {
		http.Error(w, "invalid permission", http.StatusBadRequest)
		return
	}
	
	if err := rt.app.StorageBackend().UpdateCollaborator(r.Context(), flowID, userID, req.Permission); err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			rt.sendError(w, err, http.StatusNotFound)
		} else {
			rt.sendError(w, err, http.StatusInternalServerError)
		}
		return
	}
	
	// Reload to return updated object
	collabs, _ := rt.app.StorageBackend().ListCollaborators(r.Context(), flowID)
	for _, c := range collabs {
		if c.UserID == userID {
			rt.sendJSON(w, c)
			return
		}
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleRemoveCollaborator(w http.ResponseWriter, r *http.Request, flowID, userID string) {
	if !rt.requireRole(w, r, auth.RoleMember) {
		return
	}

	if err := rt.app.StorageBackend().RemoveCollaborator(r.Context(), flowID, userID); err != nil {
		rt.sendError(w, err, http.StatusNotFound)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}
