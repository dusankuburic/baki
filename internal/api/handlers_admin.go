package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type AdminHandler struct {
	backend  storageif.StorageBackend
	security *SecurityConfig
}

func NewAdminHandler(backend storageif.StorageBackend, security *SecurityConfig) *AdminHandler {
	return &AdminHandler{backend: backend, security: security}
}

func (h *AdminHandler) handleAdminUserList(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}
	users, err := h.backend.ListUsers(r.Context())
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, users)
}

func (h *AdminHandler) handleAdminUserRole(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	id := chi.URLParam(r, "id")

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	// Prevent demoting the last admin
	if auth.Role(req.Role) != auth.RoleAdmin {
		admins, err := h.backend.ListAdmins(r.Context())
		if err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		if len(admins) == 1 && admins[0].ID == id {
			render.Error(w, fmt.Errorf("cannot demote the last administrator"), http.StatusConflict)
			return
		}
	}

	if err := h.backend.UpdateUserRole(r.Context(), id, auth.Role(req.Role)); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *AdminHandler) handleMigrationStart(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	render.Error(w, fmt.Errorf("migration service not yet autonomous"), http.StatusNotImplemented)
}

func (h *AdminHandler) handleMigrationStatus(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	render.JSON(w, map[string]any{
		"running": false,
		"result":  nil,
	})
}
