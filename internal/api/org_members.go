package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// orgInviteEmails sends invite emails off the request path, capped and drained at shutdown.
var orgInviteEmails = lifecycle.NewBounded("org_invite_email", maxConcurrentInviteEmails)

// maxConcurrentInviteEmails caps concurrent invite SMTP dials.
const maxConcurrentInviteEmails = 8

// orgMemberMutationResponse is a membership change reply; sharedCredentialDisabled is true when the change disabled the org's shared Azure credential.
type orgMemberMutationResponse struct {
	Status                   string `json:"status" example:"ok"`
	SharedCredentialDisabled bool   `json:"sharedCredentialDisabled,omitempty"`
}

const (
	lenderReasonDemoted        = "lender_demoted"
	lenderReasonRemoved        = "lender_removed"
	lenderReasonAccountDeleted = "lender_account_deleted"
)

// disableLentCredential disables the org's shared Azure credentials userID lent when userID stops
// being an admin of orgID; registry rows stay (needs_credentials). Best effort: failures are logged.
func disableLentCredential(r *http.Request, rec *audit.Recorder, providers *service.ProviderService, orgID, userID, reason string) bool {
	if providers == nil || !providers.OrgKeysAvailable() {
		return false
	}
	keys, err := providers.DisableOrgAzureCredentialsSavedBy(orgID, userID)
	if err != nil {
		logger.FromContext(r.Context()).Warn("org: disabling a departed admin's shared credentials failed", "org", orgID, "error", err)
		return false
	}
	for _, key := range keys {
		auditLentCredentialDisabled(r, rec, orgID, key, userID, reason)
	}
	return len(keys) > 0
}

// auditLentCredentialDisabled records one disabled credential; meta carries the user id because the reason names only the relationship.
func auditLentCredentialDisabled(r *http.Request, rec *audit.Recorder, orgID, key, userID, reason string) {
	meta := map[string]string{"op": "disable", "provider": key, "reason": reason, "user": userID}
	if connID, ok := ai.ParseAzureConnectionKey(key); ok {
		meta["connectionId"] = connID
	}
	rec.Record(r.Context(), orgAuditEntry(audit.ActionKeysSave, orgID, meta))
}

// @Summary      List org members
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Members"
// @Router       /api/orgs/{id}/members [get]
func (h *OrgHandler) handleOrgMemberList(w http.ResponseWriter, r *http.Request) {
	// The membership check already loaded the org with its roster.
	org := h.authorizeOrg(w, r, service.OrgRead)
	if org == nil {
		return
	}
	render.JSON(w, org.Members)
}

// @Summary      Remove organization member
// @Description  Removes a user from an organization. Only organization admins can remove members.
// @Tags         organization
// @Param        id path string true "id"
// @Param        userId path string true "userId"
// @Produce      json
// @Success      200 {object} orgMemberMutationResponse "OK; sharedCredentialDisabled is true when the removed member was the lender of the org's shared Azure AI Foundry credential and it was disabled"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      409 {object} map[string]string "Conflict"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/orgs/{id}/members/{userId} [delete]
func (h *OrgHandler) handleOrgMemberRemove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "userId")

	// Admins remove anyone, and a member may leave: only a plain role refusal lets a caller remove themselves, never
	// a token without the scope or an organisation that cannot be read.
	actor := auth.ActorFromContext(r.Context())
	if _, err := h.authz.AuthorizeOrg(r.Context(), actor, service.OrgAdmin, id); err != nil {
		leaving := actor != nil && actor.UserID == userID
		if !leaving || !errors.Is(err, service.ErrPermissionDenied) || errors.Is(err, service.ErrScopeDenied) {
			orgDenied(w, err, service.OrgAdmin)
			return
		}
	}

	if err := h.orgSvc.RemoveMember(r.Context(), id, userID); err != nil {
		if errors.Is(err, collaboration.ErrLastAdmin) {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionOrgMemberRemove, id, map[string]string{"targetUser": userID}))
	// A removed admin who lent the shared credential takes it along.
	disabled := disableLentCredential(r, h.audit, h.providers, id, userID, lenderReasonRemoved)
	render.JSON(w, orgMemberMutationResponse{Status: "ok", SharedCredentialDisabled: disabled})
}

// @Summary      List pending invites for an organization
// @Tags         orgs
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      401 {object} object "Unauthorized"
// @Router       /api/orgs/{id}/invites [get]
func (h *OrgHandler) handleOrgInviteList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Kept on the role check: Can's OrgAdmin would demand the write scope, refusing the read-only tokens this GET admits.
	if !h.requireAdminRole(w, r) {
		return
	}
	invites, err := h.orgSvc.ListInvites(r.Context(), id)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, invites)
}

// handleOrgInviteCreate creates a pending invite; the raw token is returned
// exactly once in this response — only its hash is persisted.
// @Summary      Create org invite
// @Description  handleOrgInviteCreate creates a pending invite for an email to join the organisation. Only org admins may create invites. The raw invite token is returned exactly once in this response — only its hash is persisted, so it cannot be recovered later. The caller is responsible for delivering it to the invitee (e.g. via an emailed accept link).
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       json
// @Produce      json
// @Success      201 {object} map[string]interface{} "Invite"
// @Router       /api/orgs/{id}/invites [post]
func (h *OrgHandler) handleOrgInviteCreate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)
	org := h.authorizeOrg(w, r, service.OrgAdmin) // its name goes into the invite email
	if org == nil {
		return
	}

	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	email, err := validateEmail(req.Email)
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	role := auth.Role(req.Role)
	if role == "" {
		role = auth.RoleMember
	}
	if !role.IsValid() {
		render.Error(w, fmt.Errorf("invalid role %q", req.Role), http.StatusBadRequest)
		return
	}

	invite, token, err := h.orgSvc.CreateInvite(r.Context(), id, email, role, userID, collaboration.DefaultInviteTTL)
	if err != nil {
		if errors.Is(err, storageif.ErrOrgInviteExists) {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionOrgInviteCreate, id, map[string]string{"email": email, "role": string(role)}))

	// Best-effort invite email: after the commit (a rolled-back invite must not be mailed) and off the request
	// path. SendOrgInvite is up to a 30 s SMTP round trip, and post-commit hooks run before the response is
	// complete, so sending inline in the hook held the reply for the whole dial. The invite and the token in this
	// response stand even when the mail is dropped.
	if h.email != nil {
		orgName := org.Name
		txhook.Register(r.Context(), func(ctx context.Context) {
			log := logger.FromContext(ctx) // the sender keeps the request's log attributes, not its context
			send := func() {
				if err := h.email.SendOrgInvite(context.Background(), email, orgName, token); err != nil {
					log.Error("sending org invite email failed", "error", err, "org", id, "email", email)
				}
			}
			if !orgInviteEmails.Go(send) {
				log.Warn("org invite email not sent: the mail queue is full or shutting down", "org", id)
			}
		})
	}

	render.JSON(w, map[string]any{
		"invite": invite,
		"token":  token,
	})
}

// @Summary      Revoke a pending invite
// @Tags         orgs
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      401 {object} object "Unauthorized"
// @Failure      403 {object} object "Forbidden"
// @Router       /api/orgs/{id}/invites/{inviteId} [delete]
func (h *OrgHandler) handleOrgInviteRevoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	inviteID := chi.URLParam(r, "inviteId")
	if h.authorizeOrg(w, r, service.OrgAdmin) == nil {
		return
	}

	if err := h.orgSvc.RevokeInvite(r.Context(), id, inviteID); err != nil {
		if errors.Is(err, collaboration.ErrInviteNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionOrgInviteRevoke, id, map[string]string{"inviteId": inviteID}))
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Accept an invite
// @Description  handleInviteAccept redeems an invite token, adding the calling user to the invite's organisation with the role specified in the invite.
// @Tags         org
// @Param        token path string true "Invite token"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Accepted"
// @Router       /api/invites/{token}/accept [post]
func (h *OrgHandler) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	userID := h.security.CallerID(r)
	userEmail := ""
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		userEmail = claims.Email
	}
	// H12: re-verify email-verified status at accept time — the JWT proves authentication, not
	// verification; an unverified account must not accept the victim's invites.
	emailVerified := false
	if h.backend != nil && userID != "" {
		if u, err := h.users.Load(r.Context(), userID); err == nil && u != nil {
			emailVerified = u.EmailVerified
		}
	}

	org, err := h.orgSvc.AcceptInvite(r.Context(), token, userID, userEmail, emailVerified)
	if err != nil {
		switch {
		case errors.Is(err, collaboration.ErrInviteNotFound):
			render.Error(w, err, http.StatusNotFound)
		case errors.Is(err, collaboration.ErrInviteExpired), errors.Is(err, collaboration.ErrInviteAlreadyAccepted):
			render.Error(w, err, http.StatusGone)
		default:
			render.Error(w, err, http.StatusInternalServerError)
		}
		return
	}

	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionOrgInviteAccept, org.ID, nil))
	render.JSON(w, org)
}

// @Summary      Change member role
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        userId path string true "User ID"
// @Accept       json
// @Produce      json
// @Success      200 {object} orgMemberMutationResponse "Updated; sharedCredentialDisabled is true when the demoted member was the lender of the org's shared Azure AI Foundry credential and it was disabled"
// @Router       /api/orgs/{id}/members/{userId}/role [put]
func (h *OrgHandler) handleOrgMemberRoleUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "userId")

	var req struct {
		Role string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	if h.authorizeOrg(w, r, service.OrgAdmin) == nil {
		return
	}
	role := auth.Role(req.Role)
	if !role.IsValid() {
		render.Error(w, fmt.Errorf("invalid role %q", req.Role), http.StatusBadRequest)
		return
	}
	if err := h.orgSvc.SetRole(r.Context(), id, userID, role); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionOrgMemberRole, id, map[string]string{"targetUser": userID, "role": req.Role}))
	disabled := false
	if role != auth.RoleAdmin {
		// A demoted admin can no longer manage the credential they lent.
		disabled = disableLentCredential(r, h.audit, h.providers, id, userID, lenderReasonDemoted)
	}
	render.JSON(w, orgMemberMutationResponse{Status: "ok", SharedCredentialDisabled: disabled})
}

// registerMemberRoutes wires the org members and invites sub-routes.
func (h *OrgHandler) registerMemberRoutes(rb *route.Builder) {
	rb.Route("/members", func(rb *route.Builder) {
		rb.Get("/", h.handleOrgMemberList)
		rb.Route("/{userId}", func(rb *route.Builder) {
			rb.Delete("/", h.handleOrgMemberRemove)
			rb.Put("/role", h.handleOrgMemberRoleUpdate)
		})
	})
	rb.Route("/invites", func(rb *route.Builder) {
		rb.Get("/", h.handleOrgInviteList)
		rb.Post("/", h.handleOrgInviteCreate, route.Idempotent())
		rb.Delete("/{inviteId}", h.handleOrgInviteRevoke)
	})
}
