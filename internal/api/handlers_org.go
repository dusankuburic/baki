package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type OrgHandler struct {
	orgSvc   *collaboration.OrgService
	backend  storageif.StorageBackend
	security *SecurityConfig
}

func NewOrgHandler(orgSvc *collaboration.OrgService, backend storageif.StorageBackend, security *SecurityConfig) *OrgHandler {
	return &OrgHandler{orgSvc: orgSvc, backend: backend, security: security}
}

func (h *OrgHandler) handleOrgList(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	orgs := h.orgSvc.ListForUser(userID)
	render.JSON(w, orgs)
}

func (h *OrgHandler) handleOrgCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	userID := h.security.CallerID(r)
	org, err := h.orgSvc.Create(req.Name, userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, org)
}

func (h *OrgHandler) handleOrgGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	org, err := h.orgSvc.Get(id)
	if err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}
	render.JSON(w, org)
}

func (h *OrgHandler) handleOrgUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if _, err := h.orgSvc.Get(id); err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}

	userID := h.security.CallerID(r)
	if !h.orgSvc.IsAdmin(id, userID) {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	render.Error(w, fmt.Errorf("not implemented"), http.StatusNotImplemented)
}

func (h *OrgHandler) handleOrgDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.orgSvc.Get(id); err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}

	userID := h.security.CallerID(r)
	if !h.orgSvc.IsAdmin(id, userID) {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	if err := h.orgSvc.Delete(id); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *OrgHandler) handleOrgMemberList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.orgSvc.Get(id); err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}
	members, err := h.orgSvc.ListMembers(id)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, members)
}

func (h *OrgHandler) handleOrgMemberAdd(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Email  string `json:"email"`
		UserID string `json:"userId"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	// Check if org exists
	if _, err := h.orgSvc.Get(id); err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}

	userID := h.security.CallerID(r)
	if !h.orgSvc.IsAdmin(id, userID) {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	
	if h.backend == nil {
		render.Error(w, fmt.Errorf("user store not available"), http.StatusServiceUnavailable)
		return
	}

	var targetID string
	if req.UserID != "" {
		targetID = req.UserID
	} else if req.Email != "" {
		u, err := h.backend.LoadUserByEmail(r.Context(), req.Email)
		if err != nil {
			render.Error(w, fmt.Errorf("user not found"), http.StatusNotFound)
			return
		}
		targetID = u.ID
	} else {
		render.Error(w, fmt.Errorf("userId or email required"), http.StatusBadRequest)
		return
	}

	if err := h.orgSvc.AddMember(id, targetID, auth.Role(req.Role)); err != nil {
		if strings.Contains(err.Error(), "already a member") {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *OrgHandler) handleOrgMemberRemove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "userId")
	
	if _, err := h.orgSvc.Get(id); err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}

	callerID := h.security.CallerID(r)
	if !h.orgSvc.IsAdmin(id, callerID) && callerID != userID {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}

	if err := h.orgSvc.RemoveMember(id, userID); err != nil {
		if strings.Contains(err.Error(), "last admin") {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *OrgHandler) handleOrgMemberRoleUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "userId")

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if _, err := h.orgSvc.Get(id); err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}

	callerID := h.security.CallerID(r)
	if !h.orgSvc.IsAdmin(id, callerID) {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	if err := h.orgSvc.SetRole(id, userID, auth.Role(req.Role)); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}
