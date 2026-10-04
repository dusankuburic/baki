package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/notify"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type OrgHandler struct {
	orgSvc    *collaboration.OrgService
	backend   storageif.StorageBackend
	knowledge *rag.KnowledgeService
	security  *SecurityConfig
	email     *mailer.Service
	// ruleProfiles is invalidated when org custom rules change so the next
	// analysis re-resolves; nil in local mode.
	ruleProfiles *service.RuleProfileResolver
	// providers serves org-shared credentials; routes answer
	// ORG_KEYSTORE_UNAVAILABLE without an org secret store.
	providers *service.ProviderService

	// notifyPolicy: strict whenever JWTEnabled (multi-user) so a tenant
	// cannot aim a webhook at the server's own network; desktop allows loopback.
	notifyPolicy notify.TargetPolicy
	// channelClient is the one guarded client shared by every channel test
	// (D-25): per-call clients leaked cloned transports' idle conns.
	channelClient *http.Client
}

func channelPolicy(security *SecurityConfig) notify.TargetPolicy {
	if security != nil && security.JWTEnabled {
		return notify.BlockInternalTargets
	}
	return notify.AllowLoopbackTargets
}

func NewOrgHandler(orgSvc *collaboration.OrgService, backend storageif.StorageBackend, knowledge *rag.KnowledgeService, security *SecurityConfig, email *mailer.Service, ruleProfiles *service.RuleProfileResolver, providers *service.ProviderService) *OrgHandler {
	return &OrgHandler{
		orgSvc:        orgSvc,
		backend:       backend,
		knowledge:     knowledge,
		security:      security,
		email:         email,
		ruleProfiles:  ruleProfiles,
		providers:     providers,
		notifyPolicy:  channelPolicy(security),
		channelClient: notify.NewChannelClient(channelPolicy(security)),
	}
}

func (h *OrgHandler) knowledgeBackendAvailable(w http.ResponseWriter) bool {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("knowledge base requires a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *OrgHandler) orgProviderKeysAvailable(w http.ResponseWriter) bool {
	if h.providers == nil || !h.providers.OrgKeysAvailable() {
		render.ErrorWithCode(w, service.ErrOrgKeyStoreUnavailable, http.StatusNotFound, "ORG_KEYSTORE_UNAVAILABLE")
		return false
	}
	return true
}

type statusResponse struct {
	Status string `json:"status" example:"ok"`
}

func (h *OrgHandler) memberLabel(ctx context.Context, userID string) string {
	if userID == "" || h.backend == nil {
		return userID
	}
	u, err := h.backend.LoadUserByID(ctx, userID)
	if err != nil || u == nil {
		return userID
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	if u.Email != "" {
		return u.Email
	}
	return userID
}

// requireMember checks membership in the org in "id"; non-member and unknown org both get 403 (no existence oracle).
func (h *OrgHandler) requireMember(w http.ResponseWriter, r *http.Request) *storageif.Organisation {
	id := chi.URLParam(r, "id")
	org, err := h.orgSvc.GetAsMember(r.Context(), id, h.security.CallerID(r))
	if err != nil {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return nil
	}
	return org
}

// requireAdminRole is requireAdmin needing only the decision: reads the membership row, not the org with its whole roster.
func (h *OrgHandler) requireAdminRole(w http.ResponseWriter, r *http.Request) bool {
	id := chi.URLParam(r, "id")
	role, err := h.orgSvc.MemberRole(r.Context(), id, h.security.CallerID(r))
	switch {
	case errors.Is(err, collaboration.ErrMemberNotFound):
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return false
	case err != nil:
		render.Error(w, err, http.StatusNotFound)
		return false
	case role != auth.RoleAdmin:
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return false
	}
	return true
}

func (h *OrgHandler) requireAdmin(w http.ResponseWriter, r *http.Request) *storageif.Organisation {
	id := chi.URLParam(r, "id")
	org, err := h.orgSvc.GetAndCheckAdmin(r.Context(), id, h.security.CallerID(r))
	if err != nil {
		if errors.Is(err, collaboration.ErrNotOrgAdmin) {
			render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
			return nil
		}
		render.Error(w, err, http.StatusNotFound)
		return nil
	}
	return org
}

// @Summary      List user's organizations
// @Description  Returns a list of organizations the currently authenticated user belongs to.
// @Tags         organization
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/orgs [get]
func (h *OrgHandler) handleOrgList(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	orgs, err := h.orgSvc.ListForUser(r.Context(), userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, orgs)
}

// @Summary      Create organization
// @Description  Creates a new organization owned by the caller, who becomes its
// @Description  first admin. Available to ANY authenticated user: org creation is
// @Description  self-serve, and the settings UI offers it to every cloud user.
// @Tags         organization
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Router       /api/orgs [post]
//
// NOTE: org creation is deliberately self-serve (any authenticated cloud user) and
// unmetered (OrgService.Create applies no quota) — a billing decision left to the product owner.
func (h *OrgHandler) handleOrgCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	userID := h.security.CallerID(r)
	org, err := h.orgSvc.Create(r.Context(), req.Name, userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgCreate, "org", org.ID, map[string]string{"name": org.Name})
	render.JSON(w, org)
}

// @Summary      Update organization
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Updated"
// @Router       /api/orgs/{id} [put]
func (h *OrgHandler) handleOrgUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	if !h.requireAdminRole(w, r) {
		return
	}
	// Create refuses an empty name; rename must too.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		render.Error(w, fmt.Errorf("organisation name is required"), http.StatusBadRequest)
		return
	}
	updated, err := h.orgSvc.Update(r.Context(), chi.URLParam(r, "id"), name)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgUpdate, "org", updated.ID, map[string]string{"name": updated.Name})
	render.JSON(w, updated)
}

// @Summary      Delete organization
// @Description  Deletes an existing organization. Only organization admins can delete.
// @Tags         organization
// @Param        id path string true "id"
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/orgs/{id} [delete]
func (h *OrgHandler) handleOrgDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminRole(w, r) {
		return
	}
	// The shared credential's row cascades with the org; the cached state is read now, released after the delete.
	release := func() {}
	if h.providers != nil {
		release = h.providers.PrepareOrgDeletion(chi.URLParam(r, "id"))
	}
	if err := h.orgSvc.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	release()
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgDelete, "org", chi.URLParam(r, "id"), nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// registerRoutes wires /api/orgs (org CRUD here; members/invites, rules,
// channels, knowledge and the org Azure registry live with their handlers)
// and the invite-accept route.
func (h *OrgHandler) registerRoutes(r chi.Router) {
	r.Route("/api/orgs", func(r chi.Router) {
		r.Get("/", h.handleOrgList)
		r.Post("/", h.handleOrgCreate)
		r.Route("/{id}", func(r chi.Router) {
			r.Put("/", h.handleOrgUpdate)
			r.Delete("/", h.handleOrgDelete)
			h.registerMemberRoutes(r)
			h.registerRuleRoutes(r)
			h.registerChannelRoutes(r)
			h.registerKnowledgeRoutes(r)
			h.registerOrgAzureRoutes(r)
		})
	})
	r.Route("/api/invites", func(r chi.Router) {
		r.Post("/{token}/accept", h.handleInviteAccept)
	})
}
