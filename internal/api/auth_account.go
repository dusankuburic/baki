package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// handleAuthDeleteAccount performs self-service account erasure (GDPR "right to be forgotten"): the
// caller confirms with their own email — guarding against stolen-token deletion — and the access token is revoked immediately.
// @Summary      Delete account (GDPR)
// @Description  Self-service account erasure.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/auth/account [delete]
func (h *AuthHandler) handleAuthDeleteAccount(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	var req struct {
		ConfirmEmail string `json:"confirmEmail"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if claims.Email == "" || !strings.EqualFold(strings.TrimSpace(req.ConfirmEmail), claims.Email) {
		render.Error(w, fmt.Errorf("confirmation email does not match the account"), http.StatusBadRequest)
		return
	}

	// The account service prepares (reads what the cascade removes), deletes, and finalizes (releases the user's
	// own credential, disables the org credentials they lent as an admin, drops the in-process caches); each
	// disabled credential is audited here, like the deletion, without the erased account's identity.
	erased := audit.WithErasedActor(r.Context())
	if err := h.accounts.DeleteAccount(r.Context(), claims.UserID, h.security.KeyScope(r), func(ref storageif.OrgKeyRef) {
		auditLentCredentialDisabled(erased, h.audit, ref.OrgID, ref.Provider, claims.UserID, lenderReasonAccountDeleted)
	}); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if tokenStr := auth.ExtractToken(r); tokenStr != "" {
		if tokClaims, err := h.security.AuthMgr.VerifyIgnoreExpiry(tokenStr); err == nil && tokClaims.ID != "" {
			h.security.AuthMgr.Revoke(tokClaims)
		}
	}

	h.audit.Record(erased, audit.Entry{Action: audit.ActionAccountDelete, ResourceType: "user", ResourceID: claims.UserID})
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Export account data
// @Description  GDPR data-subject export: profile, owned flows (with content), settings, API tokens, the complete audit history, chat conversations on owned flows, the comments and triage decisions the user authored, organisation memberships and active sessions. Content other users authored on shared flows is out of scope.
// @Tags         auth
// @Produce      json
// @Success      200 {object} map[string]interface{} "Account data"
// @Router       /api/auth/account/export [get]
func (h *AuthHandler) handleAuthExportAccount(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	filename := fmt.Sprintf("baki-account-export-%s.json", claims.UserID)

	// The export is built into a temp file: peak memory is one page of flows, and nothing reaches the client
	// until the whole export succeeded (a mid-stream error stays a clean 4xx/5xx).
	if h.exportAccountStreamed(w, r, claims.UserID, filename) {
		h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionDataExport, ResourceType: "user", ResourceID: claims.UserID})
	}
}

// exportAccountStreamed builds the export into a temp file and forwards it
// only on success; true means the response was written (log the audit event).
func (h *AuthHandler) exportAccountStreamed(w http.ResponseWriter, r *http.Request, userID, filename string) bool {
	tmp, err := os.CreateTemp("", "baki-export-*.json")
	if err != nil {
		render.Error(w, fmt.Errorf("export: create temp file: %w", err), http.StatusInternalServerError)
		return false
	}
	// os.CreateTemp already opens 0600; assert it so the owner-only mode
	// survives a future API swap with a different default umask.
	_ = os.Chmod(tmp.Name(), 0o600)
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer func() { _ = tmp.Close() }()

	if err := h.accounts.StreamExport(r.Context(), userID, tmp); err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return false
		}
		render.Error(w, err, http.StatusInternalServerError)
		return false
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		render.Error(w, fmt.Errorf("export: rewind temp file: %w", err), http.StatusInternalServerError)
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	if _, err := io.Copy(w, tmp); err != nil {
		// Headers/body may be partly written; the connection is the only signal left — log and stop. The
		// request's logger carries the caller's user_id.
		logger.FromContext(r.Context()).Warn("export: failed streaming temp file to client", "error", err)
		return false
	}
	return true
}

// handleAuthSessions lists the caller's active sessions (non-revoked,
// non-expired refresh tokens); without a token store it returns an empty list.
// @Summary      List active sessions for the current user
// @Tags         auth
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      401 {object} object "Unauthorized"
// @Router       /api/auth/sessions [get]
func (h *AuthHandler) handleAuthSessions(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	if !h.sessions.Enabled() {
		render.JSON(w, []storageif.RefreshTokenInfo{})
		return
	}

	sessions, err := h.sessions.List(r.Context(), claims.UserID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, sessions)
}

// @Summary      Revoke a session
// @Description  handleAuthSessionRevoke revokes one of the caller's own sessions (refresh tokens) by ID, e.g. to sign out a lost device.
// @Tags         auth
// @Param        id path string true "Session JTI"
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Router       /api/auth/sessions/{id} [delete]
func (h *AuthHandler) handleAuthSessionRevoke(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	if !h.sessions.Enabled() {
		render.Error(w, fmt.Errorf("session management not available"), http.StatusServiceUnavailable)
		return
	}

	id := chi.URLParam(r, "id")
	if err := h.sessions.RevokeForUser(r.Context(), id, claims.UserID); err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionSessionRevoke, ResourceType: "session", ResourceID: id})
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Issue a WebSocket connect ticket
// @Description  Returns a short-lived, single-use ticket the client exchanges for a WebSocket connection, keeping the access token out of the WS URL.
// @Tags         auth
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/ws-ticket [post]
func (h *AuthHandler) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	var userID, email string
	var role auth.Role
	// accessJTI/accessExp carry the authenticating access token's identity so the WebSocket
	// connection can re-check revocation (logout) and expiry. Empty in local/static-token mode.
	var accessJTI string
	var accessExp time.Time
	if claims != nil {
		userID, email, role = claims.UserID, claims.Email, claims.Role
		accessJTI = claims.ID
		// Populated for both JWT access tokens and PATs; carrying them into the WS ticket lets the
		// re-authz loop disconnect a revoked PAT's live socket and enforce its expiry.
		if claims.ExpiresAt != nil {
			accessExp = claims.ExpiresAt.Time
		}
	} else {
		userID, email, role = h.security.LocalUserID, h.security.LocalName, auth.RoleAdmin
	}

	ticket, _, err := h.security.AuthMgr.IssueWSTicket(userID, email, role, accessJTI, accessExp)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"ticket": ticket})
}
