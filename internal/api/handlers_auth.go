package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

type AuthHandler struct {
	tokenStore RefreshTokenStore
	backend    storageif.StorageBackend
	security   *SecurityConfig
	// ssoClient and identityStore are nil unless OIDC SSO is configured
	// (cloud mode with PAD_SSO_* set and a Postgres backend).
	ssoClient     SSOClient
	identityStore IdentityStore
	// email renders/sends transactional mail (password reset, verification).
	// Always non-nil — falls back to a log-only mailer when SMTP is unset.
	email *mailer.Service
	// authSvc owns credential verification (bcrypt timing/lockout) and
	// password-mutation business logic; see internal/service/auth.go.
	authSvc *service.AuthService
}

func NewAuthHandler(tokenStore RefreshTokenStore, backend storageif.StorageBackend, security *SecurityConfig, ssoClient SSOClient, identityStore IdentityStore, email *mailer.Service, authSvc *service.AuthService) *AuthHandler {
	return &AuthHandler{tokenStore: tokenStore, backend: backend, security: security, ssoClient: ssoClient, identityStore: identityStore, email: email, authSvc: authSvc}
}

// credentials is the shared {email, password} JSON body of the register and
// login endpoints. Extracted so the decode + email-normalization logic lives in
// one place instead of being duplicated across the two handlers.
type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// refreshCookieName is the HttpOnly cookie that carries the refresh token.
// Phase 1 of moving the refresh token out of JS-accessible storage (localStorage
// is XSS-readable): the cookie is SET on login/refresh and READ (cookie-first,
// body-fallback) on refresh/logout, so the browser auto-sends it on same-origin
// /api/auth POSTs. HttpOnly means document.cookie/XSS can't read it; SameSite=Lax
// blocks cross-site CSRF POSTs against /refresh; Path=/api/auth confines it to
// the auth endpoints. The body refreshToken field is still returned and accepted
// for backward compatibility during the frontend cutover (phase 2 stops writing
// localStorage; the sessions-UI's JS-read of the jti moves to a backend-provided
// field — see getCurrentSessionId in authStore.ts).
const refreshCookieName = "pad_refresh"

// isHTTPS reports whether the request reached the server over TLS (directly or
// via a trusted proxy's X-Forwarded-Proto). The refresh cookie is a long-lived
// credential, so Secure is set only when transport is actually encrypted —
// otherwise localhost/dev over http could never set or send it.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
		return xfp == "https"
	}
	return false
}

func setRefreshCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    token,
		Path:     "/api/auth",
		Expires:  expires,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func clearRefreshCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     "/api/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// refreshTokenFromRequest returns the refresh token from the HttpOnly cookie
// (preferred) or, for backward compatibility, the JSON body field. Returns ""
// when neither is present.
func (h *AuthHandler) refreshTokenFromRequest(r *http.Request, bodyToken string) string {
	if c, err := r.Cookie(refreshCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return bodyToken
}

// decodeCredentials reads the request body and returns the normalized email
// (lowercased/trimmed via validateEmail) together with the raw password.
//
// Two errors are returned so each caller can apply its own policy:
//   - err:      a malformed/missing JSON body (both handlers treat this as 400).
//   - emailErr: an email-validation failure. Register treats it as a 400, but
//     login ignores it and lets the user lookup decide, so that an empty or
//     malformed email yields 401 "invalid credentials" rather than revealing
//     input-validation rules (and without changing the login response shape).
func decodeCredentials(r *http.Request) (email, password string, emailErr error, err error) {
	var c credentials
	if err = json.NewDecoder(r.Body).Decode(&c); err != nil {
		return "", "", nil, err
	}
	email, emailErr = validateEmail(c.Email)
	return email, c.Password, emailErr, nil
}

// @Summary      Register a new user
// @Description  Creates a new user account with email and password.
// @Tags         auth
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      409 {object} map[string]string "Conflict"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/auth/register [post]
func (h *AuthHandler) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("registration not available in local mode"), http.StatusForbidden)
		return
	}
	if h.security.Features.DisableSignUp {
		// 403, not 404, so a closed deployment clearly signals "signups disabled"
		// rather than "endpoint moved". The frontend hides the form via
		// GET /api/system/features, so this only fires if a runner hits the API directly.
		render.Error(w, fmt.Errorf("account registration is disabled on this instance"), http.StatusForbidden)
		return
	}
	metrics.RecordAuthOp("register")
	email, password, emailErr, err := decodeCredentials(r)
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if emailErr != nil {
		render.Error(w, emailErr, http.StatusBadRequest)
		return
	}
	if err := validatePasswordStrength(password); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	user, err := h.authSvc.Register(r.Context(), email, password)
	if err != nil {
		// Let render.Error auto-map: ErrEmailExists → 409, everything else
		// (incl. ErrPasswordHashFailed and DB outages) → 500. Previously the
		// handler hard-coded 409 for every error, masking infra failures as
		// "email already in use".
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionRegister, "user", user.ID, nil)
	h.authSvc.SendVerificationEmail(r.Context(), user)
	render.JSON(w, map[string]any{
		"status": "ok",
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		},
	})
}

// @Summary      Login user
// @Description  Authenticates a user and returns access and refresh tokens.
// @Tags         auth
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/auth/login [post]
func (h *AuthHandler) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("login not available in local mode"), http.StatusForbidden)
		return
	}
	metrics.RecordAuthOp("login")
	// Login ignores the email-validation error (emailErr) and normalizes only:
	// an empty/malformed email proceeds to the lookup and returns 401, matching
	// the long-standing response shape and avoiding leaking input rules.
	email, password, _, err := decodeCredentials(r)
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	// Authenticate owns the timing-attack-safe credential check (dummy bcrypt
	// for a missing user, real bcrypt BEFORE the lock check) and the
	// failed-attempt/lockout bookkeeping — see internal/service/auth.go for
	// why this ordering must not change. The switch below only translates its
	// outcome into the exact same audit events and HTTP responses the inline
	// implementation produced.
	result := h.authSvc.Authenticate(r.Context(), email, password)
	switch result.Outcome {
	case service.LoginUserNotFound:
		logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLoginFailure, "email", email, map[string]string{"reason": "user_not_found"})
		render.Error(w, fmt.Errorf("invalid credentials"), http.StatusUnauthorized)
		return
	case service.LoginAccountLocked:
		logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLoginFailure, "user", result.User.ID, map[string]string{"reason": "locked"})
		render.Error(w, fmt.Errorf("account temporarily locked; please try again later"), http.StatusForbidden)
		return
	case service.LoginInvalidPassword:
		if result.AccountJustLocked {
			logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionAccountLock, "user", result.User.ID, nil)
		}
		logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLoginFailure, "user", result.User.ID, map[string]string{"reason": "invalid_password"})
		render.Error(w, fmt.Errorf("invalid credentials"), http.StatusUnauthorized)
		return
	case service.LoginEmailNotVerified:
		logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLoginFailure, "user", result.User.ID, map[string]string{"reason": "email_not_verified"})
		render.Error(w, fmt.Errorf("email not verified — check your inbox for a verification link"), http.StatusForbidden)
		return
	}
	user := result.User

	pair, err := h.security.AuthMgr.Issue(user.ID, user.Email, user.Role)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if h.tokenStore != nil {
		ua, ip := r.UserAgent(), middleware.ClientIP(r, h.security.TrustedProxies)
		if err := h.tokenStore.StoreRefreshToken(r.Context(), pair.RefreshID, user.ID, pair.RefreshExpiresAt, ua, ip); err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
	}

	// Set the HttpOnly refresh cookie (phase 1 of removing the refresh token
	// from JS-readable localStorage). The body field is still returned below
	// for backward compatibility with existing clients.
	setRefreshCookie(w, r, pair.RefreshToken, pair.RefreshExpiresAt)

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLogin, "user", user.ID, nil)
	render.JSON(w, pair)
}

// @Summary      Refresh access token
// @Description  Returns a new access token using a valid refresh token.
// @Tags         auth
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/auth/refresh [post]
func (h *AuthHandler) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	metrics.RecordAuthOp("refresh")
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	// Prefer the HttpOnly cookie over the body field (phase 1 of the
	// localStorage removal). The body fallback keeps existing clients working.
	refreshTok := h.refreshTokenFromRequest(r, req.RefreshToken)

	claims, err := h.security.AuthMgr.VerifyRefresh(refreshTok)
	if err != nil {
		render.Error(w, err, http.StatusUnauthorized)
		return
	}

	if h.tokenStore == nil {
		// In JWT mode a nil token store is a misconfiguration: the replay/
		// rotation safeguards (single-use enforcement, cross-user mismatch
		// check, replay-triggered session revocation) all depend on it. Fail
		// closed rather than silently skipping them — otherwise a stolen
		// refresh token can be replayed indefinitely. Local (non-JWT) mode
		// has no token store by design and refreshes statelessly.
		if h.security.JWTEnabled {
			logger.Error("refresh requested in JWT mode without a token store (misconfiguration); rejecting",
				"user_id", claims.UserID, "token_id", claims.ID)
			render.Error(w, fmt.Errorf("session management unavailable — please contact an administrator"), http.StatusUnauthorized)
			return
		}
	} else {
		// Use the atomic verify-and-revoke to eliminate the race window
		// between separate VerifyRefresh and RevokeRefreshToken calls.
		// Two concurrent requests with the same jti: only one succeeds
		// because the UPDATE...RETURNING is atomic at the DB level.
		info, err := h.tokenStore.VerifyAndRevokeRefreshToken(r.Context(), claims.ID)
		if err != nil {
			if errors.Is(err, storageif.ErrTokenAlreadyRevoked) {
				logger.Warn("detected refresh token replay: revoking all sessions", "userID", claims.UserID, "tokenID", claims.ID)
				if err := h.tokenStore.RevokeUserRefreshTokens(r.Context(), claims.UserID); err != nil {
					logger.Error("failed to revoke all sessions after replay detection", "error", err, "userID", claims.UserID)
				}
				render.Error(w, fmt.Errorf("session compromised — please log in again"), http.StatusUnauthorized)
				return
			}
			logger.Error("failed to verify-and-revoke refresh token", "error", err, "tokenID", claims.ID)
			render.Error(w, fmt.Errorf("failed to process token refresh"), http.StatusInternalServerError)
			return
		}
		// A1.3: Cross-check the DB-verified owner against the JWT claims to
		// prevent a forged token (different UserID in claims) from rotating
		// another user's refresh token into a new session.
		if info.UserID != claims.UserID {
			logger.Warn("refresh token user mismatch", "claimsUserID", claims.UserID, "dbUserID", info.UserID, "tokenID", claims.ID)
			render.Error(w, fmt.Errorf("session compromised — please log in again"), http.StatusUnauthorized)
			return
		}
	}

	// Reload the user from the DB so the new token reflects the CURRENT role
	// and email, not whatever was baked into the refresh-token claims.
	issueRole := claims.Role
	issueEmail := claims.Email
	if h.backend != nil {
		user, err := h.backend.LoadUserByID(r.Context(), claims.UserID)
		if err != nil {
			// A1.2: Fail closed — a deleted or non-existent user must NOT be
			// able to refresh tokens indefinitely from stale JWT claims.
			logger.Warn("refresh for deleted/missing user", "userID", claims.UserID, "error", err)
			render.Error(w, fmt.Errorf("account not found — please log in again"), http.StatusUnauthorized)
			return
		}
		// A1.1: Locked accounts must not be able to refresh their way back in.
		if user.LockedUntil != nil && time.Now().UTC().Before(*user.LockedUntil) {
			logger.Warn("refresh blocked for locked user", "userID", claims.UserID)
			render.Error(w, fmt.Errorf("account is temporarily locked"), http.StatusUnauthorized)
			return
		}
		issueRole = user.Role
		issueEmail = user.Email
	}

	pair, err := h.security.AuthMgr.Issue(claims.UserID, issueEmail, issueRole)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if h.tokenStore != nil {
		ua, ip := r.UserAgent(), middleware.ClientIP(r, h.security.TrustedProxies)
		if err := h.tokenStore.StoreRefreshToken(r.Context(), pair.RefreshID, claims.UserID, pair.RefreshExpiresAt, ua, ip); err != nil {
			logger.Error("failed to store new refresh token", "error", err, "userID", claims.UserID)
			render.Error(w, fmt.Errorf("failed to store token"), http.StatusInternalServerError)
			return
		}
	}

	// Rotate the HttpOnly cookie to the newly-issued refresh token.
	setRefreshCookie(w, r, pair.RefreshToken, pair.RefreshExpiresAt)

	render.JSON(w, pair)
}

// @Summary      Get current user info
// @Description  Returns information about the currently authenticated user.
// @Tags         auth
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Router       /api/auth/me [get]
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
			resp["createdAt"] = user.CreatedAt.UTC().Format(time.RFC3339)
		}
	}
	render.JSON(w, resp)
}

// handleAuthUpdateProfile lets a user set their own display name and avatar
// URL. Both fields are optional and may be cleared by sending an empty string.
// @Summary      Update profile
// @Description  handleAuthUpdateProfile lets a user set their own display name and avatar URL. Both fields are optional and may be cleared by sending an empty string.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/auth/profile [put]
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
	if !decodeBody(w, r, &req) {
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

// handleAuthDeleteAccount performs self-service account erasure (GDPR "right to
// be forgotten"). It requires the caller to confirm by supplying their own
// email — auth is Bearer (not cookies), but the confirmation still guards
// against stolen-token / drive-by deletion. The caller's current access token
// is revoked immediately so the erased account can't keep acting until its
// short-lived JWT would have expired.
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
	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
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

	if err := h.backend.DeleteUser(r.Context(), claims.UserID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if tokenStr := auth.ExtractToken(r); tokenStr != "" {
		if tokClaims, err := h.security.AuthMgr.VerifyIgnoreExpiry(tokenStr); err == nil && tokClaims.ID != "" {
			h.security.AuthMgr.Revoke(tokClaims)
		}
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionAccountDelete, "user", claims.UserID, nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleAuthExportAccount returns a data-subject access / portability bundle for
// the caller's own account.
// @Summary      Export account data
// @Description  GDPR data-subject export of everything stored about the user.
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
	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}
	filename := fmt.Sprintf("baki-account-export-%s.json", claims.UserID)

	// When the backend can stream the export (the Postgres backend, which
	// offloads flow content to blob storage), build it into a temp file so peak
	// memory is one page of flows rather than every flow's content at once. The
	// temp file also preserves the "complete or fail" guarantee: nothing reaches
	// the client until the whole export succeeded, so a mid-stream error still
	// becomes a clean 4xx/5xx.
	if streamer, ok := h.backend.(userDataStreamer); ok {
		if h.exportAccountStreamed(w, r, streamer, claims.UserID, filename) {
			logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionDataExport, "user", claims.UserID, nil)
		}
		return
	}

	export, err := h.backend.ExportUserData(r.Context(), claims.UserID)
	if err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionDataExport, "user", claims.UserID, nil)

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	render.JSON(w, export)
}

// userDataStreamer is implemented by backends that can stream a data-subject
// export incrementally (currently the Postgres backend). Optional: backends
// without it fall back to the buffered ExportUserData path.
type userDataStreamer interface {
	ExportUserDataTo(ctx context.Context, userID string, w io.Writer) error
}

// exportAccountStreamed builds the export into a temp file, then forwards it to
// the client only on success. Returns true when the response was written
// successfully (so the caller should log the audit event).
func (h *AuthHandler) exportAccountStreamed(w http.ResponseWriter, r *http.Request, streamer userDataStreamer, userID, filename string) bool {
	tmp, err := os.CreateTemp("", "baki-export-*.json")
	if err != nil {
		render.Error(w, fmt.Errorf("export: create temp file: %w", err), http.StatusInternalServerError)
		return false
	}
	// os.CreateTemp already opens 0600; assert it explicitly so the data-subject
	// export's owner-only mode is visible at the call site and survives a future
	// swap to an API with a different default umask.
	_ = os.Chmod(tmp.Name(), 0o600)
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer func() { _ = tmp.Close() }()

	if err := streamer.ExportUserDataTo(r.Context(), userID, tmp); err != nil {
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
		// Headers/body may be partly written; the connection is the only signal
		// left. Log and stop — can't turn this into a clean HTTP error.
		logger.Warn("export: failed streaming temp file to client", "user_id", userID, "error", err)
		return false
	}
	return true
}

// handleAuthSessions lists the caller's active sessions (non-revoked,
// non-expired refresh tokens). In local (non-JWT) mode, or if no token store
// is configured, it returns an empty list.
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

// @Summary      Logout user
// @Description  Logs out the current user. In stateless JWT mode, this is primarily a client-side operation.
// @Tags         auth
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Router       /api/auth/logout [post]
func (h *AuthHandler) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	metrics.RecordAuthOp("logout")
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn("logout: failed to decode body, proceeding without refresh token revocation", "error", err)
	}

	// Prefer the HttpOnly cookie (phase 1); fall back to the body for compat.
	refreshTok := h.refreshTokenFromRequest(r, req.RefreshToken)
	if refreshTok != "" && h.tokenStore != nil {
		claims, err := h.security.AuthMgr.VerifyRefresh(refreshTok)
		if err == nil {
			if err := h.tokenStore.RevokeRefreshToken(r.Context(), claims.ID); err != nil {
				logger.Error("failed to revoke refresh token during logout", "error", err, "tokenID", claims.ID)
			}
		}
	}

	// Clear the HttpOnly refresh cookie.
	clearRefreshCookie(w, r)

	if tokenStr := auth.ExtractToken(r); tokenStr != "" {
		if claims, err := h.security.AuthMgr.VerifyIgnoreExpiry(tokenStr); err == nil && claims.ID != "" {
			h.security.AuthMgr.Revoke(claims)
		}
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLogout, "", "", nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Change password
// @Description  Changes the password for the currently authenticated user.
// @Tags         auth
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/auth/change-password [post]
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
	if !decodeBody(w, r, &req) {
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

	if err := h.authSvc.ChangePassword(r.Context(), claims.UserID, req.OldPassword, req.NewPassword); err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidOldPassword):
			render.Error(w, err, http.StatusUnauthorized)
		case errors.Is(err, service.ErrUserLookupFailed):
			render.Error(w, err, http.StatusNotFound)
		default:
			render.Error(w, err, http.StatusInternalServerError)
		}
		return
	}

	if h.tokenStore != nil {
		if err := h.tokenStore.RevokeUserRefreshTokens(r.Context(), claims.UserID); err != nil {
			logger.Error("failed to revoke user refresh tokens after password change", "error", err, "userID", claims.UserID)
			render.Error(w, fmt.Errorf("password changed but failed to invalidate other sessions"), http.StatusInternalServerError)
			return
		}
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionPasswordChange, "user", claims.UserID, nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleAuthForgotPassword issues a password-reset token and emails its link.
// It always responds 200 with the same body whether or not the email exists, so
// the endpoint cannot be used to enumerate accounts.
// @Summary      Request password reset
// @Description  handleAuthForgotPassword issues a password-reset token and emails its link. It always responds 200 with the same body whether or not the email exists, so the endpoint cannot be used to enumerate accounts.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/auth/forgot-password [post]
func (h *AuthHandler) handleAuthForgotPassword(w http.ResponseWriter, r *http.Request) {
	if !h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("password reset not available in local mode"), http.StatusForbidden)
		return
	}
	metrics.RecordAuthOp("forgot_password")
	var req struct {
		Email string `json:"email"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	ok := map[string]string{"status": "ok"}
	email, err := validateEmail(req.Email)
	if err != nil || h.backend == nil {
		render.JSON(w, ok) // never reveal validation/availability details
		return
	}

	user, err := h.authSvc.RequestPasswordReset(r.Context(), email)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if user == nil {
		render.JSON(w, ok) // unknown email — respond identically
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionPasswordChange, "user", user.ID, map[string]string{"step": "reset_requested"})
	render.JSON(w, ok)
}

// handleAuthResetPassword redeems a reset token and sets a new password,
// revoking all of the user's existing sessions.
// @Summary      Reset password with token
// @Description  handleAuthResetPassword redeems a reset token and sets a new password, revoking all of the user's existing sessions.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/auth/reset-password [post]
func (h *AuthHandler) handleAuthResetPassword(w http.ResponseWriter, r *http.Request) {
	if !h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("password reset not available in local mode"), http.StatusForbidden)
		return
	}
	metrics.RecordAuthOp("reset_password")
	var req struct {
		Token       string `json:"token"`
		NewPassword string `json:"newPassword"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Token == "" {
		render.Error(w, fmt.Errorf("token is required"), http.StatusBadRequest)
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

	userID, err := h.authSvc.ResetPassword(r.Context(), req.Token, req.NewPassword)
	if err != nil {
		if errors.Is(err, service.ErrInvalidResetToken) {
			render.Error(w, err, http.StatusBadRequest)
		} else {
			render.Error(w, err, http.StatusInternalServerError)
		}
		return
	}
	if h.tokenStore != nil {
		if err := h.tokenStore.RevokeUserRefreshTokens(r.Context(), userID); err != nil {
			logger.Error("failed to revoke sessions after password reset", "error", err, "userID", userID)
		}
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionPasswordChange, "user", userID, map[string]string{"step": "reset_completed"})
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleAuthVerifyEmail redeems an email-verification token and marks the user's
// email verified.
// @Summary      Verify email address
// @Description  handleAuthVerifyEmail redeems an email-verification token and marks the user's email verified.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/auth/verify-email [post]
func (h *AuthHandler) handleAuthVerifyEmail(w http.ResponseWriter, r *http.Request) {
	if !h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("email verification not available in local mode"), http.StatusForbidden)
		return
	}
	metrics.RecordAuthOp("verify_email")
	var req struct {
		Token string `json:"token"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Token == "" {
		render.Error(w, fmt.Errorf("token is required"), http.StatusBadRequest)
		return
	}
	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}
	if _, err := h.authSvc.VerifyEmail(r.Context(), req.Token); err != nil {
		if errors.Is(err, service.ErrInvalidVerifyToken) {
			render.Error(w, err, http.StatusBadRequest)
		} else {
			render.Error(w, err, http.StatusInternalServerError)
		}
		return
	}
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
	// accessJTI/accessExp carry the authenticating access token's identity so
	// the eventual WebSocket connection can re-check revocation (logout) and
	// enforce the access token's expiry. Empty in local/static-token mode.
	var accessJTI string
	var accessExp time.Time
	if claims != nil {
		userID, email, role = claims.UserID, claims.Email, claims.Role
		accessJTI = claims.ID
		// claims.ID and claims.ExpiresAt are populated for both JWT access
		// tokens (JTI + 15-min expiry) and personal access tokens (a stable
		// pat:<id> JTI + the PAT's expiry, set by ClaimsForPAT in
		// verifyAPIToken). Carrying them into the WS ticket lets the re-authz
		// loop disconnect a revoked PAT's live socket and enforce the PAT's
		// expiry — previously PATs left these empty, so IsRevoked("") never
		// fired and the expiry timer was never armed.
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
