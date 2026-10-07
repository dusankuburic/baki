package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type SharingHandler struct {
	backend  storageif.StorageBackend
	flowSvc  *service.FlowService
	collabs  *service.CollaboratorService
	security *SecurityConfig
	audit    *audit.Recorder
}

func NewSharingHandler(backend storageif.StorageBackend, flowSvc *service.FlowService, collabs *service.CollaboratorService, security *SecurityConfig, recorder *audit.Recorder) *SharingHandler {
	return &SharingHandler{backend: backend, flowSvc: flowSvc, collabs: collabs, security: security, audit: recorder}
}

// @Summary      List flow collaborators
// @Description  Returns a list of users who have access to the specified flow. Any user with at least viewer access to the flow can view its collaborators.
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
	limit, ok := clampListLimit(w, r.URL.Query().Get("limit"))
	if !ok {
		return
	}
	if h.backend == nil {
		render.JSON(w, []storageif.Collaborator{})
		return
	}
	collabs, err := h.collabs.List(r.Context(), flowID, limit)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
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
	var req struct {
		Email      string `json:"email"`
		UserID     string `json:"userId"`
		Permission string `json:"permission"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	if req.UserID == "" && req.Email == "" {
		render.Error(w, fmt.Errorf("userId or email is required"), http.StatusBadRequest)
		return
	}
	u, err := h.collabs.ResolveUser(r.Context(), req.UserID, req.Email)
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

	collab, err := h.collabs.Add(r.Context(), flowID, u, perm)
	if err != nil {
		if errors.Is(err, service.ErrAlreadyCollaborator) {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowShare, ResourceType: "flow", ResourceID: flowID, Meta: map[string]string{"targetUser": u.ID, "permission": perm}})
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

	if err := h.collabs.Update(r.Context(), flowID, userID, req.Permission); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowShare, ResourceType: "flow", ResourceID: flowID, Meta: map[string]string{"targetUser": userID, "permission": req.Permission, "action": "update"}})
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
	// A removal is allowed if the caller may manage the flow's sharing
	// (FlowShare: owner, org admin, or admin-tier collaborator) OR is removing
	// themselves, so it is decided here rather than by requireFlow on the route.
	// The FlowShare check must be side-effect-free here: a response-writing
	// helper combined with the self-removal branch proceeding would double-write
	// the response (corrupting it while the row is still deleted).
	canManage := true
	if h.security.JWTEnabled {
		canManage = h.flowSvc.CheckFlowPermission(r.Context(), flowID, service.FlowShare) == nil
	}
	if !canManage && h.security.CallerID(r) != userID {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return
	}
	if err := h.collabs.Remove(r.Context(), flowID, userID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowShare, ResourceType: "flow", ResourceID: flowID, Meta: map[string]string{"targetUser": userID, "action": "remove"}})
	render.JSON(w, map[string]string{"status": "ok"})
}

// Mount registers the flow-collaborator sub-routes. The flow in the path is
// authorized before the handler runs: reading the list needs FlowRead,
// changing it FlowShare (the owner, an org admin, or a collaborator granted
// the admin tier). A removal decides for itself, because anyone may remove
// themselves. Without the server-side store the list is empty and the
// changes answer 503.
func (h *SharingHandler) Mount(rb *route.Builder) {
	share := requireFlow(h.flowSvc, h.security, service.FlowShare, "flowId")
	rb.Route("/api/flows/{flowId}/collaborators", func(rb *route.Builder) {
		rb.With(requireFlow(h.flowSvc, h.security, service.FlowRead, "flowId")).Get("/", h.handleCollaboratorList)
		rb.With(share).Post("/", h.handleCollaboratorAdd, route.Needs(capability.ServerStore), route.Idempotent())
		rb.Route("/{userId}", func(rb *route.Builder) {
			rb.Delete("/", h.handleCollaboratorRemove, route.Needs(capability.ServerStore))
			rb.With(share).Put("/", h.handleCollaboratorUpdate, route.Needs(capability.ServerStore))
		})
	})
}
