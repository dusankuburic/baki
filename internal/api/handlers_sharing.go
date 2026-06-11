package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type SharingHandler struct {
	backend  storageif.StorageBackend
	flowSvc  *service.FlowService
	security *SecurityConfig
}

func NewSharingHandler(backend storageif.StorageBackend, flowSvc *service.FlowService, security *SecurityConfig) *SharingHandler {
	return &SharingHandler{backend: backend, flowSvc: flowSvc, security: security}
}

func (h *SharingHandler) handleCollaboratorList(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowId")
	if _, ok := resolveFlow(w, r, h.flowSvc, h.security, flowID, "viewer"); !ok {
		return
	}
	if h.backend == nil {
		render.JSON(w, []storageif.Collaborator{})
		return
	}
	collabs, err := h.backend.ListCollaborators(r.Context(), flowID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, collabs)
}

func (h *SharingHandler) handleCollaboratorAdd(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowId")
	if !requireFlowOwner(w, r, h.backend, h.security, flowID) {
		return
	}
	var req struct {
		Email      string `json:"email"`
		UserID     string `json:"userId"`
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage not available"), http.StatusServiceUnavailable)
		return
	}

	var u *storageif.User
	var err error
	if req.UserID != "" {
		u, err = h.backend.LoadUserByID(r.Context(), req.UserID)
	} else if req.Email != "" {
		u, err = h.backend.LoadUserByEmail(r.Context(), req.Email)
	} else {
		render.Error(w, fmt.Errorf("userId or email is required"), http.StatusBadRequest)
		return
	}

	if err != nil {
		render.Error(w, fmt.Errorf("user not found"), http.StatusNotFound)
		return
	}

	perm := req.Permission
	if perm == "" {
		perm = "viewer"
	}
	if perm != "viewer" && perm != "editor" && perm != "admin" {
		render.Error(w, fmt.Errorf("invalid permission: %s", perm), http.StatusBadRequest)
		return
	}

	collab := &storageif.Collaborator{
		UserID:     u.ID,
		Email:      u.Email,
		Permission: perm,
		GrantedAt:  time.Now().UTC(),
	}

	if err := h.backend.AddCollaborator(r.Context(), flowID, collab); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, collab)
}

func (h *SharingHandler) handleCollaboratorUpdate(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowId")
	userID := chi.URLParam(r, "userId")
	if !requireFlowOwner(w, r, h.backend, h.security, flowID) {
		return
	}
	var req struct {
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage not available"), http.StatusServiceUnavailable)
		return
	}

	if err := h.backend.UpdateCollaborator(r.Context(), flowID, userID, req.Permission); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"permission": req.Permission})
}

func (h *SharingHandler) handleCollaboratorRemove(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowId")
	userID := chi.URLParam(r, "userId")
	// A removal is allowed if the caller owns the flow OR is removing themselves.
	// Use the side-effect-free isFlowOwner here: requireFlowOwner writes a 403 to
	// the response on failure, which — combined with the self-removal branch
	// proceeding — would double-write the response (corrupting it while the row
	// is still deleted).
	owner, err := isFlowOwner(r, h.backend, h.security, flowID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	if !owner && h.security.CallerID(r) != userID {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return
	}
	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage not available"), http.StatusServiceUnavailable)
		return
	}
	if err := h.backend.RemoveCollaborator(r.Context(), flowID, userID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *SharingHandler) handleSharingRoute(w http.ResponseWriter, r *http.Request) {
}
