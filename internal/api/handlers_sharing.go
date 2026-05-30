package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// requireFlowOwner verifies the caller owns flowID before allowing collaborator
// management. In local/Tauri mode (single user) all operations are trusted.
// Returns false (and writes the response) when the flow is missing or not owned
// by the caller.
func (rt *Router) requireFlowOwner(w http.ResponseWriter, r *http.Request, flowID string) bool {
	if !rt.jwtEnabled {
		return true
	}
	backend := rt.app.StorageBackend()
	if backend == nil {
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
		return false
	}
	flow, err := backend.LoadFlow(r.Context(), flowID)
	if err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			rt.sendError(w, err, http.StatusInternalServerError)
		}
		return false
	}
	if flow.OwnerID != rt.callerID(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// permRank orders flow collaborator permissions so they can be compared with ">=".
var permRank = map[string]int{
	"viewer": 10,
	"editor": 20,
	"admin":  30,
}

// orgRoleToPermRank maps an org member's role to the equivalent flow permission
// rank for org-owned flows: admins manage, members edit, everyone else reads.
func orgRoleToPermRank(role auth.Role) int {
	switch role {
	case auth.RoleAdmin:
		return permRank["admin"]
	case auth.RoleMember:
		return permRank["editor"]
	default:
		return permRank["viewer"]
	}
}

// requireFlowAccess authorizes the caller to act on flowID at >= minPerm
// ("viewer" | "editor" | "admin"). Local/Tauri mode is single-user and always
// passes. Cloud mode: the owner always passes; a member of the flow's org passes
// if their org role grants enough; otherwise an explicit collaborator grant must
// meet minPerm. Writes 404 when the flow is missing, 403 when under-privileged.
func (rt *Router) requireFlowAccess(w http.ResponseWriter, r *http.Request, flowID, minPerm string) bool {
	if !rt.jwtEnabled {
		return true
	}
	backend := rt.app.StorageBackend()
	if backend == nil {
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
		return false
	}
	if flowID == "" {
		rt.sendError(w, fmt.Errorf("flow ID is required"), http.StatusBadRequest)
		return false
	}
	flow, err := backend.LoadFlow(r.Context(), flowID)
	if err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			rt.sendError(w, err, http.StatusInternalServerError)
		}
		return false
	}
	need := permRank[minPerm]
	caller := rt.callerID(r)
	if flow.OwnerID == caller {
		return true
	}
	if flow.OrganizationID != "" {
		if role, err := rt.orgSvc.MemberRole(flow.OrganizationID, caller); err == nil {
			if orgRoleToPermRank(role) >= need {
				return true
			}
		}
	}
	collabs, err := backend.ListCollaborators(r.Context(), flowID)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return false
	}
	for _, c := range collabs {
		if c.UserID == caller && permRank[c.Permission] >= need {
			return true
		}
	}
	http.Error(w, "Forbidden", http.StatusForbidden)
	return false
}

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

// @Summary List flow collaborators
// @Description Returns a list of users who have access to the specified flow. Only the flow owner can view collaborators.
// @Tags sharing
// @Produce json
// @Param flowId path string true "Flow ID"
// @Success 200 {array} interfaces.Collaborator
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flows/{flowId}/collaborators [get]
func (rt *Router) handleListCollaborators(w http.ResponseWriter, r *http.Request, flowID string) {
	if !rt.requireRole(w, r, auth.RoleMember) {
		return
	}
	if !rt.requireFlowOwner(w, r, flowID) {
		return
	}
	backend := rt.app.StorageBackend()
	if backend == nil {
		rt.sendJSON(w, []*storageif.Collaborator{})
		return
	}
	collabs, err := backend.ListCollaborators(r.Context(), flowID)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, collabs)
}

// @Summary Add flow collaborator
// @Description Grants access to a flow to another user. Only the flow owner can add collaborators.
// @Tags sharing
// @Accept json
// @Produce json
// @Param flowId path string true "Flow ID"
// @Param request body object{email=string,userId=string,permission=string} true "Add Collaborator Request"
// @Success 200 {object} interfaces.Collaborator
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flows/{flowId}/collaborators [post]
func (rt *Router) handleAddCollaborator(w http.ResponseWriter, r *http.Request, flowID string) {
	if !rt.requireRole(w, r, auth.RoleMember) {
		return
	}
	if !rt.requireFlowOwner(w, r, flowID) {
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

	backend := rt.app.StorageBackend()
	if backend == nil {
		http.Error(w, "sharing is not available in local mode", http.StatusForbidden)
		return
	}

	var userID, email string

	if req.Email != "" {
		// Look up user by email
		u, err := backend.LoadUserByEmail(r.Context(), req.Email)
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
		u, err := backend.LoadUserByID(r.Context(), req.UserID)
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
	if err := backend.AddCollaborator(r.Context(), flowID, c); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, c)
}

// @Summary Update flow collaborator permission
// @Description Updates the permission level for an existing flow collaborator. Only the flow owner can update permissions.
// @Tags sharing
// @Accept json
// @Produce json
// @Param flowId path string true "Flow ID"
// @Param userId path string true "User ID"
// @Param request body object{permission=string} true "Update Permission Request"
// @Success 200 {object} interfaces.Collaborator
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flows/{flowId}/collaborators/{userId} [put]
func (rt *Router) handleUpdateCollaborator(w http.ResponseWriter, r *http.Request, flowID, userID string) {
	if !rt.requireRole(w, r, auth.RoleMember) {
		return
	}
	if !rt.requireFlowOwner(w, r, flowID) {
		return
	}
	backend := rt.app.StorageBackend()
	if backend == nil {
		http.Error(w, "sharing is not available in local mode", http.StatusForbidden)
		return
	}
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

	if err := backend.UpdateCollaborator(r.Context(), flowID, userID, req.Permission); err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			rt.sendError(w, err, http.StatusNotFound)
		} else {
			rt.sendError(w, err, http.StatusInternalServerError)
		}
		return
	}

	// Reload to return updated object
	collabs, _ := backend.ListCollaborators(r.Context(), flowID)
	for _, c := range collabs {
		if c.UserID == userID {
			rt.sendJSON(w, c)
			return
		}
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Remove flow collaborator
// @Description Revokes a user's access to a specific flow. Only the flow owner can remove collaborators.
// @Tags sharing
// @Produce json
// @Param flowId path string true "Flow ID"
// @Param userId path string true "User ID"
// @Success 200 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flows/{flowId}/collaborators/{userId} [delete]
func (rt *Router) handleRemoveCollaborator(w http.ResponseWriter, r *http.Request, flowID, userID string) {
	if !rt.requireRole(w, r, auth.RoleMember) {
		return
	}
	if !rt.requireFlowOwner(w, r, flowID) {
		return
	}
	backend := rt.app.StorageBackend()
	if backend == nil {
		http.Error(w, "sharing is not available in local mode", http.StatusForbidden)
		return
	}

	if err := backend.RemoveCollaborator(r.Context(), flowID, userID); err != nil {
		rt.sendError(w, err, http.StatusNotFound)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}
