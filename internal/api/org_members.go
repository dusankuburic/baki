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
	"pad-analyzer/internal/events"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

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

// lendsSharedCredential reports whether userID lent orgID shared Azure credentials, which leaving the admin role
// disables: the reply says so at once, the disabling itself follows the membership change's commit
// (SubscribeLentCredentialRevocation). Read before the change, which may deliver at once where there is no
// transaction to wait for. Best effort: a failed read is logged and reports false.
func lendsSharedCredential(ctx context.Context, providers *service.ProviderService, orgID, userID string) bool {
	if providers == nil || !providers.OrgKeysAvailable() {
		return false
	}
	keys, err := providers.OrgAzureCredentialsLentBy(orgID, userID)
	if err != nil {
		logger.FromContext(ctx).Warn("org: reading a departing admin's shared credentials failed", "org", orgID, "error", err)
		return false
	}
	return len(keys) > 0
}

// SubscribeLentCredentialRevocation disables the org's shared Azure credentials a member lent once they stop
// being an admin of it, removed or demoted. Async: only after the membership change has committed, never for one
// that rolled back. The delivery keeps the request's actor and request info, so the audit rows name who made the
// change. It must not be lost (the credential would stay in use, and the response already said it was disabled),
// so it runs inline when the bus's runner is full: one keystore delete.
func SubscribeLentCredentialRevocation(bus *eventbus.Bus, providers *service.ProviderService, rec *audit.Recorder) {
	eventbus.Subscribe(bus, eventbus.Async, "lent-credential-revocation", func(ctx context.Context, e events.OrgMemberChanged) {
		revokeLentCredentials(ctx, rec, providers, e)
	}, eventbus.InlineWhenSaturated(),
		eventbus.Local("a keystore write with audit rows: it must happen once, and the shared keystore makes it visible to every replica"))
}

// revokeLentCredentials is SubscribeLentCredentialRevocation's delivery.
func revokeLentCredentials(ctx context.Context, rec *audit.Recorder, providers *service.ProviderService, e events.OrgMemberChanged) {
	switch {
	case e.Kind == events.OrgMemberRemoved:
		disableLentCredential(ctx, rec, providers, e.OrgID, e.UserID, lenderReasonRemoved)
	case e.Kind == events.OrgMemberRoleChanged && auth.Role(e.Role) != auth.RoleAdmin:
		// A demoted admin can no longer manage the credential they lent.
		disableLentCredential(ctx, rec, providers, e.OrgID, e.UserID, lenderReasonDemoted)
	}
}

// disableLentCredential disables the org's shared Azure credentials userID lent when userID stops
// being an admin of orgID; registry rows stay (needs_credentials). Best effort: failures are logged.
func disableLentCredential(ctx context.Context, rec *audit.Recorder, providers *service.ProviderService, orgID, userID, reason string) {
	if providers == nil || !providers.OrgKeysAvailable() {
		return
	}
	keys, err := providers.DisableOrgAzureCredentialsSavedBy(orgID, userID)
	if err != nil {
		logger.FromContext(ctx).Warn("org: disabling a departed admin's shared credentials failed", "org", orgID, "error", err)
		return
	}
	for _, key := range keys {
		auditLentCredentialDisabled(ctx, rec, orgID, key, userID, reason)
	}
}

// auditLentCredentialDisabled records one disabled credential; meta carries the user id because the reason names only the relationship.
func auditLentCredentialDisabled(ctx context.Context, rec *audit.Recorder, orgID, key, userID, reason string) {
	meta := map[string]string{"op": "disable", "provider": key, "reason": reason, "user": userID}
	if connID, ok := ai.ParseAzureConnectionKey(key); ok {
		meta["connectionId"] = connID
	}
	rec.Record(ctx, orgAuditEntry(audit.ActionKeysSave, orgID, meta))
}

// renderMemberChangeError answers a refused removal or role change: a user who is not a member is a 404, like an
// unknown organisation (which the generic mapping already answers so), and taking the last admin away is a 409.
func renderMemberChangeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, collaboration.ErrMemberNotFound):
		render.Error(w, err, http.StatusNotFound)
	case errors.Is(err, collaboration.ErrLastAdmin):
		render.Error(w, err, http.StatusConflict)
	default:
		render.Error(w, err, http.StatusInternalServerError)
	}
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

	// A removed admin who lent the shared credential takes it along (after the commit).
	disabled := lendsSharedCredential(r.Context(), h.providers, id, userID)
	if err := h.orgSvc.RemoveMember(r.Context(), id, userID); err != nil {
		renderMemberChangeError(w, err)
		return
	}
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionOrgMemberRemove, id, map[string]string{"targetUser": userID}))
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

	// Best-effort invite email: queued after the commit (a rolled-back invite must not be mailed) on the mail
	// runner, so the SMTP round trip never holds the reply. The invite and the token in this response stand even
	// when the mail is dropped.
	if h.email != nil {
		invite := mailer.OrgInvite.With(mailer.OrgInviteData{OrgName: org.Name, Token: token})
		txhook.Register(r.Context(), func(ctx context.Context) {
			if err := h.email.Enqueue(ctx, email, invite); err != nil {
				logger.FromContext(ctx).Warn("org invite email not queued", "error", err, "org", id)
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
// @Failure      404 {object} map[string]string "Unknown organisation, or the user is not a member of it"
// @Failure      409 {object} map[string]string "The change would leave the organisation without an admin"
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
	disabled := false
	if role != auth.RoleAdmin {
		// A demoted admin's lent credential is disabled after the commit.
		disabled = lendsSharedCredential(r.Context(), h.providers, id, userID)
	}
	if err := h.orgSvc.SetRole(r.Context(), id, userID, role); err != nil {
		renderMemberChangeError(w, err)
		return
	}
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionOrgMemberRole, id, map[string]string{"targetUser": userID, "role": req.Role}))
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
