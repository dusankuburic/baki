package api

import (
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

// @Summary      List flow collaborators
// @Description  Returns a list of users who have access to the specified flow. Only the flow owner can view collaborators.
// @Tags         sharing
// @Param        flowId path string true "flowId"
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flows/{flowId}/collaborators [get]
func (h *SharingHandler) handleCollaboratorList(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowId")
	if _, ok := resolveFlow(w, r, h.flowSvc, h.security, flowID, "viewer"); !ok {
		return
	}
	limit, ok := clampListLimit(w, r.URL.Query().Get("limit"))
	if !ok {
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
	if len(collabs) > limit {
		collabs = collabs[:limit]
	}
	render.JSON(w, collabs)
}

// @Summary      Add flow collaborator
// @Description  Grants access to a flow to another user. Only the flow owner can add collaborators.
// @Tags         sharing
// @Param        flowId path string true "flowId"
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flows/{flowId}/collaborators [post]
func (h *SharingHandler) handleCollaboratorAdd(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowId")
	// Sharing is managed by anyone with the "admin" rank on the flow: the
	// owner, an org admin, or a collaborator granted the admin tier.
	if !requireFlowPerm(w, r, h.flowSvc, h.security, flowID, "admin") {
		return
	}
	var req struct {
		Email      string `json:"email"`
		UserID     string `json:"userId"`
		Permission string `json:"permission"`
	}
	if !decodeBody(w, r, &req) {
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
		// Anti-enumeration: return the same 200 "ok" response as a successful
		// add rather than 404 "user not found". A 404 let an admin probe whether
		// an arbitrary email/userID is registered; an indistinguishable response
		// (matching the forgot-password flow's design) closes that oracle. The
		// collaborator is NOT added for an unknown target.
		render.JSON(w, map[string]string{"status": "ok"})
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
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowShare, "flow", flowID, map[string]string{"targetUser": u.ID, "permission": perm})
	render.JSON(w, collab)
}

// @Summary      Update flow collaborator permission
// @Description  Updates the permission level for an existing flow collaborator. Only the flow owner can update permissions.
// @Tags         sharing
// @Param        flowId path string true "flowId"
// @Param        userId path string true "userId"
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flows/{flowId}/collaborators/{userId} [put]
func (h *SharingHandler) handleCollaboratorUpdate(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowId")
	userID := chi.URLParam(r, "userId")
	if !requireFlowPerm(w, r, h.flowSvc, h.security, flowID, "admin") {
		return
	}
	var req struct {
		Permission string `json:"permission"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	if req.Permission != "viewer" && req.Permission != "editor" && req.Permission != "admin" {
		render.Error(w, fmt.Errorf("invalid permission: %s", req.Permission), http.StatusBadRequest)
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
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowShare, "flow", flowID, map[string]string{"targetUser": userID, "permission": req.Permission, "action": "update"})
	render.JSON(w, map[string]string{"permission": req.Permission})
}

// @Summary      Remove flow collaborator
// @Description  Revokes a user's access to a specific flow. Only the flow owner can remove collaborators.
// @Tags         sharing
// @Param        flowId path string true "flowId"
// @Param        userId path string true "userId"
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flows/{flowId}/collaborators/{userId} [delete]
func (h *SharingHandler) handleCollaboratorRemove(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowId")
	userID := chi.URLParam(r, "userId")
	// A removal is allowed if the caller has the "admin" rank on the flow
	// (owner, org admin, or admin-tier collaborator) OR is removing themselves.
	// The admin check must be side-effect-free here: a response-writing helper
	// combined with the self-removal branch proceeding would double-write the
	// response (corrupting it while the row is still deleted).
	canManage := true
	if h.security.JWTEnabled {
		err := h.flowSvc.CheckFlowPermission(r.Context(), flowID, h.security.CallerID(r), "admin")
		canManage = err == nil
	}
	if !canManage && h.security.CallerID(r) != userID {
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
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowShare, "flow", flowID, map[string]string{"targetUser": userID, "action": "remove"})
	render.JSON(w, map[string]string{"status": "ok"})
}
