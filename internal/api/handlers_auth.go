package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/metrics"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type AuthHandler struct {
	tokenStore RefreshTokenStore
	backend    storageif.StorageBackend
	security   *SecurityConfig
	// ssoClient and identityStore are nil unless OIDC SSO is configured
	// (cloud mode with PAD_SSO_* set and a Postgres backend).
	ssoClient     SSOClient
	identityStore IdentityStore
}

func NewAuthHandler(tokenStore RefreshTokenStore, backend storageif.StorageBackend, security *SecurityConfig, ssoClient SSOClient, identityStore IdentityStore) *AuthHandler {
	return &AuthHandler{tokenStore: tokenStore, backend: backend, security: security, ssoClient: ssoClient, identityStore: identityStore}
}

func (h *AuthHandler) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("registration not available in local mode"), http.StatusForbidden)
		return
	}
	metrics.RecordAuthOp("register")
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	email, err := validateEmail(req.Email)
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := validatePasswordStrength(req.Password); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	hashed, err := auth.HashPassword(req.Password)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	user := &storageif.User{
		ID:       uuid.NewString(),
		Email:    email,
		Password: hashed,
		// CreateUser atomically promotes the very first user to RoleAdmin
		// (count + insert in one transaction/lock), so there is always an
		// initial administrator without a racy check-then-create here.
		Role: auth.RoleMember,
	}

	if err := h.backend.CreateUser(r.Context(), user); err != nil {
		render.Error(w, err, http.StatusConflict)
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionRegister, "user", user.ID, nil)
	render.JSON(w, map[string]any{
		"status": "ok",
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		},
	})
}

func (h *AuthHandler) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("login not available in local mode"), http.StatusForbidden)
		return
	}
	metrics.RecordAuthOp("login")
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	email, _ := validateEmail(req.Email) // normalization only here; load handles mismatch

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	user, err := h.backend.LoadUserByEmail(r.Context(), email)
	if err != nil {
		// Log failed attempt for non-existent user (but return generic error)
		logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLoginFailure, "email", email, map[string]string{"reason": "user_not_found"})

		// SECURITY: perform a dummy check to prevent timing attacks that
		// leak whether an email exists. bcrypt is expensive; if we
		// return immediately for non-existent users, attackers can
		// enumerate your user list.
		auth.CheckPasswordHash(req.Password, "$2a$12$R.8j9.v.8j9.v.8j9.v.8j9.v.8j9.v.8j9.v.8j9.v.8j9.v.8j9.v")

		render.Error(w, fmt.Errorf("invalid credentials"), http.StatusUnauthorized)
		return
	}

	// SECURITY: run bcrypt FIRST so the response time is identical
	// regardless of whether the account is locked. Without this, a locked
	// account returns in ~1ms (DB lookup only) while an unlocked account
	// with a wrong password takes ~50ms (DB + bcrypt), allowing attackers
	// to distinguish locked from unlocked accounts by timing.
	passwordValid := auth.CheckPasswordHash(req.Password, user.Password)

	// Check lock status AFTER bcrypt so timing is consistent.
	if user.LockedUntil != nil && time.Now().UTC().Before(*user.LockedUntil) {
		logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLoginFailure, "user", user.ID, map[string]string{"reason": "locked"})
		render.Error(w, fmt.Errorf("account temporarily locked; please try again later"), http.StatusForbidden)
		return
	}

	if !passwordValid {
		// Increment failed attempts
		user.FailedLoginAttempts++
		if user.FailedLoginAttempts >= 5 {
			// Lock for 15 minutes
			until := time.Now().UTC().Add(15 * time.Minute)
			user.LockedUntil = &until
			logger.Warn("account locked due to too many failed login attempts", "email", user.Email, "userID", user.ID)
			logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionAccountLock, "user", user.ID, nil)
		}
		if err := h.backend.SaveUser(r.Context(), user); err != nil {
			logger.Error("failed to update failed login attempts", "error", err, "userID", user.ID)
		}

		logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLoginFailure, "user", user.ID, map[string]string{"reason": "invalid_password"})
		render.Error(w, fmt.Errorf("invalid credentials"), http.StatusUnauthorized)
		return
	}

	// Reset failed attempts on successful login
	if user.FailedLoginAttempts > 0 || user.LockedUntil != nil {
		user.FailedLoginAttempts = 0
		user.LockedUntil = nil
		if err := h.backend.SaveUser(r.Context(), user); err != nil {
			logger.Error("failed to reset failed login attempts", "error", err, "userID", user.ID)
		}
	}

	pair, err := h.security.AuthMgr.Issue(user.ID, user.Email, user.Role)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if h.tokenStore != nil {
		if err := h.tokenStore.StoreRefreshToken(r.Context(), pair.RefreshID, user.ID, pair.RefreshExpiresAt); err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLogin, "user", user.ID, nil)
	render.JSON(w, pair)
}

func (h *AuthHandler) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	metrics.RecordAuthOp("refresh")
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	claims, err := h.security.AuthMgr.VerifyRefresh(req.RefreshToken)
	if err != nil {
		render.Error(w, err, http.StatusUnauthorized)
		return
	}

	if h.tokenStore != nil {
		// Atomically revoke the old token. If the token was already revoked
		// (concurrent refresh, replay attack), RowsAffected=0 and we detect it
		// here — no separate check-then-revoke race window.
		if err := h.tokenStore.RevokeRefreshToken(r.Context(), claims.ID); err != nil {
			if errors.Is(err, storageif.ErrTokenAlreadyRevoked) {
				logger.Warn("detected refresh token replay: revoking all sessions", "userID", claims.UserID, "tokenID", claims.ID)
				if err := h.tokenStore.RevokeUserRefreshTokens(r.Context(), claims.UserID); err != nil {
					logger.Error("failed to revoke all sessions after replay detection", "error", err, "userID", claims.UserID)
				}
				render.Error(w, fmt.Errorf("session compromised — please log in again"), http.StatusUnauthorized)
				return
			}
			logger.Error("failed to revoke old refresh token during refresh", "error", err, "tokenID", claims.ID)
			render.Error(w, fmt.Errorf("failed to process token refresh"), http.StatusInternalServerError)
			return
		}
	}

	// Reload the user from the DB so the new token reflects the CURRENT role
	// and email, not whatever was baked into the refresh-token claims. Without
	// this, a demoted admin keeps issuing admin-level access tokens until their
	// refresh token expires (up to 24h).
	issueRole := claims.Role
	issueEmail := claims.Email
	if h.backend != nil {
		if user, err := h.backend.LoadUserByID(r.Context(), claims.UserID); err == nil {
			issueRole = user.Role
			issueEmail = user.Email
		}
	}

	pair, err := h.security.AuthMgr.Issue(claims.UserID, issueEmail, issueRole)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if h.tokenStore != nil {
		if err := h.tokenStore.StoreRefreshToken(r.Context(), pair.RefreshID, claims.UserID, pair.RefreshExpiresAt); err != nil {
			logger.Error("failed to store new refresh token", "error", err, "userID", claims.UserID)
			render.Error(w, fmt.Errorf("failed to store token"), http.StatusInternalServerError)
			return
		}
	}

	render.JSON(w, pair)
}

func (h *AuthHandler) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		if !h.security.JWTEnabled {
			render.JSON(w, map[string]string{
				"id":    h.security.LocalUserID,
				"email": h.security.LocalName,
				"role":  string(auth.RoleAdmin),
			})
			return
		}
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	resp := map[string]string{
		"id":    claims.UserID,
		"email": claims.Email,
		"role":  string(claims.Role),
	}
	if h.backend != nil {
		if user, err := h.backend.LoadUserByID(r.Context(), claims.UserID); err == nil {
			resp["displayName"] = user.DisplayName
			resp["avatarUrl"] = user.AvatarURL
		}
	}
	render.JSON(w, resp)
}

// handleAuthUpdateProfile lets a user set their own display name and avatar
// URL. Both fields are optional and may be cleared by sending an empty string.
func (h *AuthHandler) handleAuthUpdateProfile(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	var req struct {
		DisplayName string `json:"displayName"`
		AvatarURL   string `json:"avatarUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(req.DisplayName) > 100 {
		render.Error(w, fmt.Errorf("display name must be at most 100 characters"), http.StatusBadRequest)
		return
	}
	if len(req.AvatarURL) > 2048 {
		render.Error(w, fmt.Errorf("avatar URL must be at most 2048 characters"), http.StatusBadRequest)
		return
	}
	if req.AvatarURL != "" {
		parsedURL, err := url.Parse(req.AvatarURL)
		if err != nil || (parsedURL.Scheme != "https" && parsedURL.Scheme != "http") {
			render.Error(w, fmt.Errorf("avatar URL must be a valid http(s) URL"), http.StatusBadRequest)
			return
		}
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	if err := h.backend.UpdateUserProfile(r.Context(), claims.UserID, req.DisplayName, req.AvatarURL); err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionProfileUpdate, "user", claims.UserID, nil)
	render.JSON(w, map[string]string{
		"id":          claims.UserID,
		"email":       claims.Email,
		"role":        string(claims.Role),
		"displayName": req.DisplayName,
		"avatarUrl":   req.AvatarURL,
	})
}

// handleAuthSessions lists the caller's active sessions (non-revoked,
// non-expired refresh tokens). In local (non-JWT) mode, or if no token store
// is configured, it returns an empty list.
func (h *AuthHandler) handleAuthSessions(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	if h.tokenStore == nil {
		render.JSON(w, []storageif.RefreshTokenInfo{})
		return
	}

	sessions, err := h.tokenStore.ListUserRefreshTokens(r.Context(), claims.UserID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []storageif.RefreshTokenInfo{}
	}
	render.JSON(w, sessions)
}

// handleAuthSessionRevoke revokes one of the caller's own sessions (refresh
// tokens) by ID, e.g. to sign out a lost device.
func (h *AuthHandler) handleAuthSessionRevoke(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	if h.tokenStore == nil {
		render.Error(w, fmt.Errorf("session management not available"), http.StatusServiceUnavailable)
		return
	}

	id := chi.URLParam(r, "id")
	if err := h.tokenStore.RevokeRefreshTokenForUser(r.Context(), id, claims.UserID); err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionSessionRevoke, "session", id, nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *AuthHandler) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	metrics.RecordAuthOp("logout")
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn("logout: failed to decode body, proceeding without refresh token revocation", "error", err)
	}

	if req.RefreshToken != "" && h.tokenStore != nil {
		claims, err := h.security.AuthMgr.VerifyRefresh(req.RefreshToken)
		if err == nil {
			if err := h.tokenStore.RevokeRefreshToken(r.Context(), claims.ID); err != nil {
				logger.Error("failed to revoke refresh token during logout", "error", err, "tokenID", claims.ID)
			}
		}
	}

	if tokenStr := auth.ExtractToken(r); tokenStr != "" {
		if claims, err := h.security.AuthMgr.VerifyIgnoreExpiry(tokenStr); err == nil && claims.ID != "" {
			h.security.AuthMgr.Revoke(claims)
		}
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLogout, "", "", nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *AuthHandler) handleAuthChangePassword(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if err := validatePasswordStrength(req.NewPassword); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	user, err := h.backend.LoadUserByID(r.Context(), claims.UserID)
	if err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}

	if !auth.CheckPasswordHash(req.OldPassword, user.Password) {
		render.Error(w, fmt.Errorf("invalid old password"), http.StatusUnauthorized)
		return
	}

	hashed, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if err := h.backend.UpdateUserPassword(r.Context(), user.ID, hashed); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if h.tokenStore != nil {
		if err := h.tokenStore.RevokeUserRefreshTokens(r.Context(), user.ID); err != nil {
			logger.Error("failed to revoke user refresh tokens after password change", "error", err, "userID", user.ID)
			render.Error(w, fmt.Errorf("password changed but failed to invalidate other sessions"), http.StatusInternalServerError)
			return
		}
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionPasswordChange, "user", claims.UserID, nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// validateEmail rejects malformed or over-long addresses and returns a
// normalized (lowercased) version. RFC 5321 caps an address at 254 chars;
// net/mail.ParseAddress catches the structural cases the old
// `strings.Contains(email, "@")` check let through.
func validateEmail(email string) (string, error) {
	if len(email) > 254 {
		return "", fmt.Errorf("email too long")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return "", fmt.Errorf("invalid email format")
	}
	return strings.ToLower(addr.Address), nil
}

// validatePasswordStrength enforces a basic password policy. The upper bound is
// 72 bytes because bcrypt (used by auth.HashPassword) silently ignores anything
// past 72 — without this cap, the tail of a long password would not actually
// protect the account.
func validatePasswordStrength(pw string) error {
	if len(pw) < 12 {
		return fmt.Errorf("password must be at least 12 characters")
	}
	if len(pw) > 72 {
		return fmt.Errorf("password must be at most 72 characters")
	}
	var hasLower, hasUpper, hasDigit, hasSymbol bool
	for _, c := range pw {
		switch {
		case unicode.IsLower(c):
			hasLower = true
		case unicode.IsUpper(c):
			hasUpper = true
		case unicode.IsDigit(c):
			hasDigit = true
		default:
			hasSymbol = true
		}
	}
	classes := 0
	for _, present := range []bool{hasLower, hasUpper, hasDigit, hasSymbol} {
		if present {
			classes++
		}
	}
	if classes < 3 {
		return fmt.Errorf("password must include at least 3 of: lowercase, uppercase, digit, symbol")
	}
	return nil
}

func (h *AuthHandler) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	var userID, email string
	var role auth.Role
	if claims != nil {
		userID, email, role = claims.UserID, claims.Email, claims.Role
	} else {
		userID, email, role = h.security.LocalUserID, h.security.LocalName, auth.RoleAdmin
	}

	ticket, _, err := h.security.AuthMgr.IssueWSTicket(userID, email, role)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"ticket": ticket})
}
