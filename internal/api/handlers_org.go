package api

import (
	"encoding/json"
	"errors"
	"net/http"
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
	for _, m := range org.Members {
		if m.UserID == callerID && (m.Role == auth.RoleAdmin || org.OwnerID == callerID) {
			return true
		}
	}
	// Org owner always has access
	if org.OwnerID == callerID {
		return true
	}
	http.Error(w, "Forbidden", http.StatusForbidden)
	return false
}

func (rt *Router) handleOrgList(w http.ResponseWriter, r *http.Request) {
	// Always list orgs for the authenticated caller; ignore any userId in the body.
	userID := rt.callerID(r)
	orgs := rt.orgSvc.ListForUser(userID)
	if orgs == nil {
		orgs = []*interfaces.Organisation{}
	}
	rt.sendJSON(w, orgs)
}

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

func (rt *Router) handleOrgMemberAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrgID  string    `json:"orgId"`
		UserID string    `json:"userId"`
		Role   auth.Role `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireOrgAdmin(w, r, req.OrgID) {
		return
	}
	if err := rt.orgSvc.AddMember(req.OrgID, req.UserID, req.Role); err != nil {
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

func (rt *Router) handleOrgMemberRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrgID  string `json:"orgId"`
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireOrgAdmin(w, r, req.OrgID) {
		return
	}
	if err := rt.orgSvc.RemoveMember(req.OrgID, req.UserID); err != nil {
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

func (rt *Router) handleOrgMemberSetRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrgID  string    `json:"orgId"`
		UserID string    `json:"userId"`
		Role   auth.Role `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireOrgAdmin(w, r, req.OrgID) {
		return
	}
	if err := rt.orgSvc.SetRole(req.OrgID, req.UserID, req.Role); err != nil {
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
