package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"time"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// ── Organisation CI credentials (ADR 0019) ────────────────────────────
//
// An org admin mints the HMAC keys the organisation's CI runners sign POST /api/integrations/ci with. The credential
// string is in the create answer only; afterwards the list shows its last four characters.

// ciCredentialOut is the client shape of a credential, without its secret.
type ciCredentialOut struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SecretHint string `json:"secretHint"`
	CreatedBy  string `json:"createdBy"`
	// CreatedByName is the minter's display name or email; it and createdBy are "" once the account is gone.
	CreatedByName string     `json:"createdByName"`
	CreatedAt     time.Time  `json:"createdAt"`
	ExpiresAt     *time.Time `json:"expiresAt"`
	LastUsedAt    *time.Time `json:"lastUsedAt"`
}

// ciCredentialCreated is the create answer: the credential and, this once, the credential string.
type ciCredentialCreated struct {
	ciCredentialOut
	Credential string `json:"credential"`
}

func (h *OrgHandler) toCICredentialOut(ctx context.Context, c *storageif.CICredential) ciCredentialOut {
	out := ciCredentialOut{ID: c.ID, Name: c.Name, SecretHint: c.SecretHint, CreatedBy: c.CreatedBy,
		CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, LastUsedAt: c.LastUsedAt}
	if c.CreatedBy == "" || h.backend == nil || h.users == nil {
		return out
	}
	// The erasure clears created_by, but an account erased by a release before it (a rollback window) left its id
	// in the row: the list shows such a minter as gone, not by a raw id.
	u, err := h.users.Load(ctx, c.CreatedBy)
	switch {
	case errors.Is(err, storageif.ErrNotFound):
		out.CreatedBy = ""
	case err == nil && u != nil:
		out.CreatedByName = cmp.Or(u.DisplayName, u.Email)
	}
	return out
}

type orgCICredentialsRequest struct {
	OrgID string `path:"id"`
}

// @Summary      List an organisation's CI credentials
// @Description  Org admins only. Every credential, expired ones included until the retention sweep removes them 30 days after expiry; never the secret, only its last four characters (secretHint). expiresAt and lastUsedAt are null for never.
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {array} ciCredentialOut "Credentials, newest first"
// @Failure      403 {object} render.ErrorResponse "Not an admin of the organisation"
// @Router       /api/orgs/{id}/ci-credentials [get]
func (h *OrgHandler) listCICredentials(ctx context.Context, req orgCICredentialsRequest) ([]ciCredentialOut, error) {
	creds, err := h.ci.ListCredentials(ctx, req.OrgID)
	if err != nil {
		return nil, err
	}
	out := make([]ciCredentialOut, len(creds))
	for i, c := range creds {
		out[i] = h.toCICredentialOut(ctx, c)
	}
	return out, nil
}

type createCICredentialRequest struct {
	OrgID string `json:"-" path:"id"`
	Name  string `json:"name"`
	// ExpiresInDays is 1..365; absent, null or 0: the credential never expires.
	ExpiresInDays *int `json:"expiresInDays"`
}

// @Summary      Mint an organisation CI credential
// @Description  Org admins only; API tokens are refused. Returns the credential string (baki_ci_<keyId>_<secret>) once, in `credential`: give it to the CI runner as PAD_CI_WEBHOOK_SECRET for `bakicli report`. name is required (at most 100 characters); expiresInDays is 1 to 365, or left out for a credential that never expires. At most 10 unexpired credentials per organisation (409 CI_CREDENTIAL_LIMIT). The server must encrypt secrets at rest (503 CI_CREDENTIALS_UNAVAILABLE without PAD_ENCRYPTION_KEY). Supports Idempotency-Key; a retry gets the same answer, credential included.
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        request body createCICredentialRequest true "{name, expiresInDays?}"
// @Accept       json
// @Produce      json
// @Success      201 {object} ciCredentialCreated "The credential, with the credential string shown once"
// @Failure      400 {object} render.ErrorResponse "Invalid name or expiry"
// @Failure      403 {object} render.ErrorResponse "Not an admin of the organisation, or an API token"
// @Failure      409 {object} render.ErrorResponse "CI_CREDENTIAL_LIMIT"
// @Failure      503 {object} render.ErrorResponse "CI_CREDENTIALS_UNAVAILABLE"
// @Router       /api/orgs/{id}/ci-credentials [post]
func (h *OrgHandler) createCICredential(ctx context.Context, req createCICredentialRequest) (ciCredentialCreated, error) {
	days := 0
	if req.ExpiresInDays != nil {
		days = *req.ExpiresInDays
	}
	createdBy := ""
	if a := auth.ActorFromContext(ctx); a != nil {
		createdBy = a.UserID
	}
	c, credential, err := h.ci.CreateCredential(ctx, req.OrgID, createdBy, req.Name, days)
	if err != nil {
		return ciCredentialCreated{}, err
	}
	h.audit.Record(ctx, orgAuditEntry(audit.ActionOrgCICredentialCreate, req.OrgID, map[string]string{"keyId": c.ID, "name": c.Name}))
	return ciCredentialCreated{ciCredentialOut: h.toCICredentialOut(ctx, c), Credential: credential}, nil
}

type revokeCICredentialRequest struct {
	OrgID  string `path:"id"`
	CredID string `path:"credId"`
}

// @Summary      Revoke an organisation CI credential
// @Description  Org admins only; API tokens are refused. The credential is deleted: a runner signing with it gets 401 from its next call on. To rotate, mint a new credential, update the CI secret, then revoke the old one.
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        credId path string true "Credential key id"
// @Success      204 "Revoked"
// @Failure      403 {object} render.ErrorResponse "Not an admin of the organisation, or an API token"
// @Failure      404 {object} render.ErrorResponse "No such credential in this organisation"
// @Router       /api/orgs/{id}/ci-credentials/{credId} [delete]
func (h *OrgHandler) revokeCICredential(ctx context.Context, req revokeCICredentialRequest) (struct{}, error) {
	if err := h.ci.RevokeCredential(ctx, req.OrgID, req.CredID); err != nil {
		return struct{}{}, err
	}
	h.audit.Record(ctx, orgAuditEntry(audit.ActionOrgCICredentialRevoke, req.OrgID, map[string]string{"keyId": req.CredID}))
	return struct{}{}, nil
}

// registerCICredentialRoutes wires the organisation CI credential sub-routes: org admins only, minting and revoking
// refused to API tokens (credential management, like /api/auth/tokens). Organisations exist with accounts, and the
// credentials live in the server store.
func (h *OrgHandler) registerCICredentialRoutes(rb *route.Builder) {
	rb.Route("/ci-credentials", func(rb *route.Builder) {
		admin := rb.With(h.requireOrg(service.OrgAdmin))
		admin.Get("/", Query(h.listCICredentials))
		// Not Idempotent: a replay would hand the one-time credential out again, past the admin check that runs
		// after the replay (to an admin since removed). A retried mint mints another credential, which the list shows.
		admin.Post("/", JSON(h.createCICredential, Status(http.StatusCreated)), route.DenyTokens())
		admin.Delete("/{credId}", Query(h.revokeCICredential, Status(http.StatusNoContent)), route.DenyTokens())
	}, route.Needs(capability.ServerStore, capability.Accounts))
}
