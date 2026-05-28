package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"pad-analyzer/internal/auth"
)

// @Summary List all users
// @Description Returns a list of all registered users. Only available to system admins.
// @Tags admin
// @Produce json
// @Success 200 {array} interfaces.User
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/admin/users/list [get]
func (rt *Router) handleAdminUserList(w http.ResponseWriter, r *http.Request) {
	if !rt.requireRole(w, r, auth.RoleAdmin) {
		return
	}

	users, err := rt.app.StorageBackend().ListUsers(r.Context())
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	// Sanitize password hashes out of the response
	for _, u := range users {
		u.Password = ""
	}

	rt.sendJSON(w, users)
}

// @Summary Set user role
// @Description Updates the system role for a specific user. Only available to system admins.
// @Tags admin
// @Accept json
// @Produce json
// @Param userId path string true "User ID"
// @Param request body object{role=string} true "Set Role Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/admin/users/{userId}/role [post]
func (rt *Router) handleAdminUserRole(w http.ResponseWriter, r *http.Request) {
	if !rt.requireRole(w, r, auth.RoleAdmin) {
		return
	}

	// Path: /api/admin/users/<userId>/role
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/admin/users/"), "/")
	if len(parts) < 2 || parts[1] != "role" {
		http.NotFound(w, r)
		return
	}
	targetUserID := parts[0]

	var req struct {
		Role auth.Role `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}

	if !req.Role.IsValid() {
		http.Error(w, "invalid role", http.StatusBadRequest)
		return
	}

	user, err := rt.app.StorageBackend().LoadUserByID(r.Context(), targetUserID)
	if err != nil {
		rt.sendError(w, err, http.StatusNotFound)
		return
	}

	user.Role = req.Role
	if err := rt.app.StorageBackend().SaveUser(r.Context(), user); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	rt.sendJSON(w, map[string]string{"status": "ok"})
}
