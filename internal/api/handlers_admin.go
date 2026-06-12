package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

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

	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	users, err := h.backend.ListUsers(r.Context(), limit, offset)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	// Fall back to a page-size approximation when a count query is unavailable.
	total, err := h.backend.CountUsers(r.Context())
	if err != nil {
		total = offset + len(users)
	}

	render.JSON(w, render.PagedResponse[*storageif.User]{
		Items:  users,
		Total:  total,
		Offset: offset,
		Limit:  limit,
	})
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
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionRoleChange, "user", id, map[string]string{"new_role": req.Role})
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *AdminHandler) handleAdminAuditList(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	filter := storageif.AuditFilter{
		UserID: q.Get("userId"),
		Action: q.Get("action"),
		Limit:  limit,
		Offset: offset,
	}

	events, err := h.backend.ListAuditEvents(r.Context(), filter)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, events)
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
