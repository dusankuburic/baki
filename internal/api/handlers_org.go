package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/storage/interfaces"
)

// callerID returns the authenticated user's ID from JWT claims (cloud mode)
// or the local user ID (local/Tauri mode).
func (rt *Router) callerID(r *http.Request) string {
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		return claims.UserID
	}
	return rt.localUserID
}

// requireOrgAdmin checks that the calling user is an admin of orgID.
// Returns false and writes 403 if not.
func (rt *Router) requireOrgAdmin(w http.ResponseWriter, r *http.Request, orgID string) bool {
	if !rt.jwtEnabled {
		return true // local mode: all operations are trusted
	}
	callerID := rt.callerID(r)
	org, err := rt.orgSvc.Get(orgID)
	if err != nil {
		if errors.Is(err, collaboration.ErrOrgNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return false
	}
	// Org owner always has access.
	if org.OwnerID == callerID {
		return true
	}
	for _, m := range org.Members {
		if m.UserID == callerID && m.Role == auth.RoleAdmin {
			return true
		}
	}
	http.Error(w, "Forbidden", http.StatusForbidden)
	return false
}

// handleOrgsRoute dispatches the REST org API:
//
//	GET    /api/orgs
//	POST   /api/orgs
//	DELETE /api/orgs/:id
//	POST   /api/orgs/:id/members
//	DELETE /api/orgs/:id/members/:userId
//	POST   /api/orgs/:id/members/:userId/role
func (rt *Router) handleOrgsRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/orgs"), "/")

	// Collection: /api/orgs
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rt.handleOrgList(w, r)
		case http.MethodPost:
			rt.handleOrgCreate(w, r)
		default:
			http.NotFound(w, r)
		}
		return
	}

	parts := strings.Split(rest, "/")
	orgID := parts[0]
	if orgID == "" {
		http.NotFound(w, r)
		return
	}

	switch {
	case len(parts) == 1:
		// /api/orgs/:id
		if r.Method == http.MethodDelete {
			rt.handleOrgDelete(w, r, orgID)
		} else {
			http.NotFound(w, r)
		}
	case len(parts) == 2 && parts[1] == "members":
		// /api/orgs/:id/members
		if r.Method == http.MethodPost {
			rt.handleOrgMemberAdd(w, r, orgID)
		} else {
			http.NotFound(w, r)
		}
	case len(parts) == 3 && parts[1] == "members":
		// /api/orgs/:id/members/:userId
		if r.Method == http.MethodDelete {
			rt.handleOrgMemberRemove(w, r, orgID, parts[2])
		} else {
			http.NotFound(w, r)
		}
	case len(parts) == 4 && parts[1] == "members" && parts[3] == "role":
		// /api/orgs/:id/members/:userId/role
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			rt.handleOrgMemberSetRole(w, r, orgID, parts[2])
		} else {
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

// @Summary List user's organizations
// @Description Returns a list of organizations the currently authenticated user belongs to.
// @Tags organization
// @Produce json
// @Success 200 {array} interfaces.Organisation
// @Failure 500 {object} map[string]string
// @Router /api/orgs [get]
func (rt *Router) handleOrgList(w http.ResponseWriter, r *http.Request) {
	// Always list orgs for the authenticated caller; ignore any userId in the body.
	userID := rt.callerID(r)
	orgs := rt.orgSvc.ListForUser(userID)
	if orgs == nil {
		orgs = []*interfaces.Organisation{}
	}
	rt.sendJSON(w, orgs)
}

// @Summary Create organization
// @Description Creates a new organization. Only available to system admins.
// @Tags organization
// @Accept json
// @Produce json
// @Param request body object{name=string} true "Create Organization Request"
// @Success 200 {object} interfaces.Organisation
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Router /api/orgs [post]
func (rt *Router) handleOrgCreate(w http.ResponseWriter, r *http.Request) {
	if !rt.requireRole(w, r, auth.RoleAdmin) {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	// Owner is always the authenticated caller — never trust client-supplied ownerId.
	ownerID := rt.callerID(r)
	org, err := rt.orgSvc.Create(req.Name, ownerID)
	if err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	rt.sendJSON(w, org)
}

// @Summary Delete organization
// @Description Deletes an existing organization. Only organization admins can delete.
// @Tags organization
// @Produce json
// @Param id path string true "Organization ID"
// @Success 200 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/orgs/{id} [delete]
func (rt *Router) handleOrgDelete(w http.ResponseWriter, r *http.Request, orgID string) {
	if !rt.requireOrgAdmin(w, r, orgID) {
		return
	}
	if err := rt.orgSvc.Delete(orgID); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, collaboration.ErrOrgNotFound) {
			code = http.StatusNotFound
		}
		rt.sendError(w, err, code)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Add organization member
// @Description Adds a user to an organization with a specific role. Only organization admins can add members.
// @Tags organization
// @Accept json
// @Produce json
// @Param id path string true "Organization ID"
// @Param request body object{email=string,userId=string,role=string} true "Add Member Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 409 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/orgs/{id}/members [post]
func (rt *Router) handleOrgMemberAdd(w http.ResponseWriter, r *http.Request, orgID string) {
	var req struct {
		Email  string    `json:"email"`
		UserID string    `json:"userId"`
		Role   auth.Role `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireOrgAdmin(w, r, orgID) {
		return
	}

	// Resolve the target user: prefer an explicit userId, otherwise look up by email.
	userID := req.UserID
	if userID == "" && req.Email != "" {
		u, err := rt.app.StorageBackend().LoadUserByEmail(r.Context(), req.Email)
		if err != nil {
			if errors.Is(err, interfaces.ErrNotFound) {
				http.Error(w, "user not found", http.StatusNotFound)
				return
			}
			rt.sendError(w, err, http.StatusInternalServerError)
			return
		}
		userID = u.ID
	}
	if userID == "" {
		http.Error(w, "email or userId is required", http.StatusBadRequest)
		return
	}
	if req.Role == "" {
		req.Role = auth.RoleMember
	}

	if err := rt.orgSvc.AddMember(orgID, userID, req.Role); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, collaboration.ErrOrgNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, collaboration.ErrAlreadyMember) {
			code = http.StatusConflict
		}
		rt.sendError(w, err, code)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Remove organization member
// @Description Removes a user from an organization. Only organization admins can remove members.
// @Tags organization
// @Produce json
// @Param id path string true "Organization ID"
// @Param userId path string true "User ID"
// @Success 200 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 409 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/orgs/{id}/members/{userId} [delete]
func (rt *Router) handleOrgMemberRemove(w http.ResponseWriter, r *http.Request, orgID, userID string) {
	if !rt.requireOrgAdmin(w, r, orgID) {
		return
	}
	if err := rt.orgSvc.RemoveMember(orgID, userID); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, collaboration.ErrOrgNotFound) || errors.Is(err, collaboration.ErrMemberNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, collaboration.ErrLastAdmin) {
			code = http.StatusConflict
		}
		rt.sendError(w, err, code)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Set organization member role
// @Description Updates the role of a user within an organization. Only organization admins can set roles.
// @Tags organization
// @Accept json
// @Produce json
// @Param id path string true "Organization ID"
// @Param userId path string true "User ID"
// @Param request body object{role=string} true "Set Role Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 409 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/orgs/{id}/members/{userId}/role [post]
func (rt *Router) handleOrgMemberSetRole(w http.ResponseWriter, r *http.Request, orgID, userID string) {
	var req struct {
		Role auth.Role `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireOrgAdmin(w, r, orgID) {
		return
	}
	if err := rt.orgSvc.SetRole(orgID, userID, req.Role); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, collaboration.ErrOrgNotFound) || errors.Is(err, collaboration.ErrMemberNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, collaboration.ErrLastAdmin) {
			code = http.StatusConflict
		}
		rt.sendError(w, err, code)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}
