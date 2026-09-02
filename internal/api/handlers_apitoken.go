package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Machine API tokens (personal access tokens). These let CI and other automation
// call the API as the issuing user without an interactive login. They require a
// storage backend (cloud mode); the raw token is returned exactly once, at
// creation, and only its hash is ever stored.

const (
	defaultAPITokenLifetimeDays = 90
	maxAPITokenLifetimeDays     = 365
)

// @Summary      Create API token
// @Tags         auth
// @Accept       json
// @Produce      json
// @Success      201 {object} map[string]interface{} "Created token"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Router       /api/auth/tokens [post]
func (h *AuthHandler) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("API tokens require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	userID := h.security.CallerID(r)
	if userID == "" {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	var req struct {
		Name          string   `json:"name"`
		ExpiresInDays int      `json:"expiresInDays"`
		Scopes        []string `json:"scopes"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	// Scope validation (R2-1): the closed set only. An empty/omitted list is
	// the backward-compatible unscoped token (full access) — minting one
	// requires no new permissions.
	for _, sc := range req.Scopes {
		if !auth.ValidScope(sc) {
			render.Error(w, fmt.Errorf("unknown scope %q (valid: read, write, chat, admin)", sc), http.StatusBadRequest)
			return
		}
	}

	raw, hash, err := auth.GenerateAPIToken()
	if err != nil {
		render.Error(w, fmt.Errorf("generate token: %w", err), http.StatusInternalServerError)
		return
	}

	tok := &storageif.APIToken{
		ID:        uuid.NewString(),
		UserID:    userID,
		Name:      req.Name,
		TokenHash: hash,
		Scopes:    req.Scopes,
		CreatedAt: time.Now().UTC(),
	}
	days := req.ExpiresInDays
	if days <= 0 {
		days = defaultAPITokenLifetimeDays
	}
	if days > maxAPITokenLifetimeDays {
		render.Error(w, fmt.Errorf("token lifetime cannot exceed %d days", maxAPITokenLifetimeDays), http.StatusBadRequest)
		return
	}
	exp := tok.CreatedAt.AddDate(0, 0, days)
	tok.ExpiresAt = &exp

	if err := h.backend.CreateAPIToken(r.Context(), tok); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionTokenCreate, "api_token", tok.ID,
		map[string]string{"name": tok.Name})

	// The raw token is shown exactly once here; afterwards only its hash exists.
	render.JSON(w, map[string]any{
		"id":        tok.ID,
		"name":      tok.Name,
		"token":     raw,
		"scopes":    tok.Scopes,
		"createdAt": tok.CreatedAt,
		"expiresAt": tok.ExpiresAt,
	})
}

// @Summary      List API tokens for the current user
// @Tags         auth
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      401 {object} object "Unauthorized"
// @Router       /api/auth/tokens [get]
func (h *AuthHandler) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("API tokens require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	userID := h.security.CallerID(r)
	if userID == "" {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	tokens, err := h.backend.ListAPITokens(r.Context(), userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if tokens == nil {
		tokens = []*storageif.APIToken{}
	}
	// APIToken.TokenHash is json:"-", so the hash is never serialized here.
	render.JSON(w, tokens)
}

// @Summary      Revoke API token
// @Tags         auth
// @Param        id path string true "Token ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Router       /api/auth/tokens/{id} [delete]
func (h *AuthHandler) handleDeleteAPIToken(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("API tokens require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	userID := h.security.CallerID(r)
	if userID == "" {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		render.Error(w, fmt.Errorf("token id is required"), http.StatusBadRequest)
		return
	}

	if err := h.backend.DeleteAPIToken(r.Context(), userID, id); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Blacklist the PAT's derived JTI so live WebSocket connections authenticated
	// via this PAT are disconnected by the WS re-authz loop (2-min ticker). PATs
	// are not JWTs, so the normal logout path (which blacklists the access JTI)
	// never fires for them — without this, a deleted PAT's open socket stays live.
	// 10 min covers ~5 re-authz cycles; no new tickets can be issued afterward
	// since the PAT row is gone (verifyAPIToken returns nil).
	if h.security.AuthMgr != nil {
		h.security.AuthMgr.RevokeJTI(auth.PATJTI(id), patRevokeBlacklistTTL)
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionTokenRevoke, "api_token", id, nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// patRevokeBlacklistTTL is how long a deleted PAT's derived JTI stays in the
// blacklist. It only needs to outlive the WS re-authz loop's interval (2 min)
// so an already-open socket sees the revocation; a few cycles of margin is
// plenty since a deleted PAT can no longer mint new WS tickets.
const patRevokeBlacklistTTL = 10 * time.Minute
