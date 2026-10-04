package api

import (
	"context"
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
	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// RefreshTokenStore tracks issued refresh tokens so they can be rotated and
// revoked (cloud mode).
type RefreshTokenStore interface {
	// userAgent and ip are captured from the issuing request so the session
	// can later be shown with a friendly device label.
	StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time, userAgent, ip string) error
	IsRefreshTokenValid(ctx context.Context, jti string) (bool, error)
	RevokeRefreshToken(ctx context.Context, jti string) error
	// VerifyAndRevokeRefreshToken atomically verifies and revokes a refresh token
	// in a single atomic operation. Returns the token info if successful,
	// or ErrTokenAlreadyRevoked if the token was already revoked/invalid.
	// This eliminates the race window between VerifyRefresh and RevokeRefreshToken.
	VerifyAndRevokeRefreshToken(ctx context.Context, jti string) (*storageif.RefreshTokenInfo, error)
	RevokeUserRefreshTokens(ctx context.Context, userID string) error
	// ListUserRefreshTokens returns active sessions for a user, for display in
	// profile/session-management UIs.
	ListUserRefreshTokens(ctx context.Context, userID string) ([]storageif.RefreshTokenInfo, error)
	// RevokeRefreshTokenForUser revokes a single session, scoped to userID so a
	// user cannot revoke another user's session.
	RevokeRefreshTokenForUser(ctx context.Context, jti, userID string) error
}

type AuthHandler struct {
	tokenStore RefreshTokenStore
	backend    storageif.StorageBackend
	security   *SecurityConfig
	// ssoClient and identityStore are nil unless OIDC SSO is configured
	// (cloud mode with PAD_SSO_* set and a Postgres backend).
	ssoClient     SSOClient
	identityStore IdentityStore
	// email sends transactional mail; always non-nil (log-only when SMTP unset).
	email *mailer.Service
	// authSvc owns credential verification (bcrypt timing/lockout); see internal/service/auth.go.
	authSvc *service.AuthService
	// onUserDeleted runs after handleAuthDeleteAccount erased the user (wired to
	// ProviderService.InvalidateScope): the SQL cascade does not reach the
	// in-process caches.
	onUserDeleted func(scope string)
	// providers releases, on account erasure, process state the SQL cascade does not
	// reach: the user's own Azure credential and every org credential they lent as
	// an admin. nil = none (bare tests, desktop).
	providers *service.ProviderService
}

func NewAuthHandler(tokenStore RefreshTokenStore, backend storageif.StorageBackend, security *SecurityConfig, ssoClient SSOClient, identityStore IdentityStore, email *mailer.Service, authSvc *service.AuthService) *AuthHandler {
	return &AuthHandler{tokenStore: tokenStore, backend: backend, security: security, ssoClient: ssoClient, identityStore: identityStore, email: email, authSvc: authSvc}
}

// OnUserDeleted registers the post-erasure hook; set once at boot in di.
func (h *AuthHandler) OnUserDeleted(fn func(scope string)) {
	h.onUserDeleted = fn
}

// SetProviderService wires the provider service account erasure releases credentials through; set once at boot.
func (h *AuthHandler) SetProviderService(p *service.ProviderService) {
	h.providers = p
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// refreshCookieName: HttpOnly keeps document.cookie/XSS out, SameSite=Lax blocks cross-site
// CSRF POSTs, Path=/api/auth confines it. The body field stays for compat.
const refreshCookieName = "pad_refresh"

// isHTTPS reports whether the request reached the server over TLS (directly or via a trusted
// proxy's X-Forwarded-Proto); the refresh cookie's Secure flag is set only for encrypted transport.
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
	// #nosec G124 -- HttpOnly and SameSite are both set; only Secure is
	// conditional, and deliberately so. The desktop build serves plain HTTP on
	// loopback, where a hard Secure:true makes the browser drop the refresh
	// cookie and breaks login entirely. isHTTPS(r) honours the proxy headers in
	// cloud mode, where it resolves to true.
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
	// #nosec G124 -- HttpOnly and SameSite are both set; only Secure is
	// conditional, and deliberately so. The desktop build serves plain HTTP on
	// loopback, where a hard Secure:true makes the browser drop the refresh
	// cookie and breaks login entirely. isHTTPS(r) honours the proxy headers in
	// cloud mode, where it resolves to true.
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

func (h *AuthHandler) refreshTokenFromRequest(r *http.Request, bodyToken string) string {
	if c, err := r.Cookie(refreshCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return bodyToken
}

// decodeCredentials returns the normalized email and raw password with two errors for caller policy:
// err is a malformed body (both 400); register answers emailErr 400, login ignores it — a malformed
// email yields 401 "invalid credentials" without revealing rules.
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
		// 403, not 404: a closed deployment clearly signals "signups disabled"; the form is hidden via /api/system/features.
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
		// render.Error auto-maps: ErrEmailExists → 409, everything else → 500.
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionRegister, "user", user.ID, nil)
	userJSON := map[string]any{
		"id":    user.ID,
		"email": user.Email,
		"role":  user.Role,
	}
	if user.EmailVerified {
		// No verification round trip pending (bootstrap admin is created verified): issue the session directly.
		pair, ok := h.issueSession(w, r, user.ID, user.Email, user.Role)
		if !ok {
			return
		}
		render.JSON(w, map[string]any{
			"status":               "ok",
			"user":                 userJSON,
			"verificationRequired": false,
			"accessToken":          pair.AccessToken,
			"refreshToken":         pair.RefreshToken,
			"expiresAt":            pair.ExpiresAt,
			"refreshId":            pair.RefreshID,
			"refreshExpiresAt":     pair.RefreshExpiresAt,
		})
		return
	}
	h.authSvc.SendVerificationEmail(r.Context(), user)
	// verificationRequired: this is NOT a session — the user must follow the emailed link first.
	render.JSON(w, map[string]any{
		"status":               "ok",
		"user":                 userJSON,
		"verificationRequired": true,
	})
}

// issueSession mints a token pair, persists the refresh token, sets the HttpOnly cookie and records
// the login audit; on failure it has written the error and returns ok=false.
func (h *AuthHandler) issueSession(w http.ResponseWriter, r *http.Request, userID, email string, role auth.Role) (*auth.TokenPair, bool) {
	pair, err := h.security.AuthMgr.Issue(userID, email, role)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return nil, false
	}

	if h.tokenStore != nil {
		ua, ip := r.UserAgent(), middleware.ClientIP(r, h.security.TrustedProxies)
		if err := h.tokenStore.StoreRefreshToken(r.Context(), pair.RefreshID, userID, pair.RefreshExpiresAt, ua, ip); err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return nil, false
		}
	}

	// Set the HttpOnly refresh cookie; callers still return the body field for backward compatibility.
	setRefreshCookie(w, r, pair.RefreshToken, pair.RefreshExpiresAt)

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLogin, "user", userID, nil)
	return pair, true
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
	// Login ignores emailErr and normalizes only (see decodeCredentials): a malformed email returns 401.
	email, password, _, err := decodeCredentials(r)
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	// Authenticate owns the timing-attack-safe credential check and the failed-attempt/lockout bookkeeping
	// (internal/service/auth.go — that ordering must not change); this switch only translates outcomes.
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
		// Machine-readable code so the login page can offer "resend the link".
		render.ErrorWithCode(w, fmt.Errorf("email not verified — check your inbox for a verification link"), http.StatusForbidden, "email_not_verified")
		return
	}
	user := result.User

	pair, ok := h.issueSession(w, r, user.ID, user.Email, user.Role)
	if !ok {
		return
	}
	// The client validates against LoginResponseSchema, which requires a `user` object — tokens alone failed validation.
	render.JSON(w, map[string]any{
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		},
		"accessToken":      pair.AccessToken,
		"refreshToken":     pair.RefreshToken,
		"expiresAt":        pair.ExpiresAt,
		"refreshId":        pair.RefreshID,
		"refreshExpiresAt": pair.RefreshExpiresAt,
	})
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

	// Prefer the HttpOnly cookie; the body fallback keeps old clients working.
	refreshTok := h.refreshTokenFromRequest(r, req.RefreshToken)

	claims, err := h.security.AuthMgr.VerifyRefresh(refreshTok)
	if err != nil {
		render.Error(w, err, http.StatusUnauthorized)
		return
	}

	if h.tokenStore == nil {
		// In JWT mode a nil token store is a misconfiguration: the replay and rotation safeguards all
		// depend on it — fail closed, else a stolen refresh token replays indefinitely.
		if h.security.JWTEnabled {
			logger.Error("refresh requested in JWT mode without a token store (misconfiguration); rejecting",
				"user_id", claims.UserID, "token_id", claims.ID)
			render.Error(w, fmt.Errorf("session management unavailable — please contact an administrator"), http.StatusUnauthorized)
			return
		}
	} else {
		// Atomic verify-and-revoke: of two concurrent requests with the same
		// jti only one succeeds (UPDATE...RETURNING is atomic at the DB).
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
		// A1.3: a forged token must not rotate another user's refresh token.
		if info.UserID != claims.UserID {
			logger.Warn("refresh token user mismatch", "claimsUserID", claims.UserID, "dbUserID", info.UserID, "tokenID", claims.ID)
			render.Error(w, fmt.Errorf("session compromised — please log in again"), http.StatusUnauthorized)
			return
		}
	}

	// Reload the user so the new token reflects the CURRENT role and email, not the refresh-token claims.
	issueRole := claims.Role
	issueEmail := claims.Email
	if h.backend != nil {
		user, err := h.backend.LoadUserByID(r.Context(), claims.UserID)
		if err != nil {
			// A1.2: fail closed — a deleted user must not refresh from stale claims.
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

	refreshTok := h.refreshTokenFromRequest(r, req.RefreshToken)
	if refreshTok != "" && h.tokenStore != nil {
		claims, err := h.security.AuthMgr.VerifyRefresh(refreshTok)
		if err == nil {
			if err := h.tokenStore.RevokeRefreshToken(r.Context(), claims.ID); err != nil {
				logger.Error("failed to revoke refresh token during logout", "error", err, "tokenID", claims.ID)
			}
		}
	}

	clearRefreshCookie(w, r)

	if tokenStr := auth.ExtractToken(r); tokenStr != "" {
		if claims, err := h.security.AuthMgr.VerifyIgnoreExpiry(tokenStr); err == nil && claims.ID != "" {
			h.security.AuthMgr.Revoke(claims)
		}
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionLogout, "", "", nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// validateEmail rejects malformed or over-long addresses and returns the
// normalized (lowercased) form; RFC 5321 caps an address at 254 chars.
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

// validatePasswordStrength enforces the password policy. The 72-byte cap is because bcrypt
// silently ignores anything past 72 — the tail would not protect the account.
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

// registerRoutes wires /api/auth (register/login/refresh, profile, password,
// sessions, account, tokens, SSO) and the WebSocket ticket route. Password,
// account, token and SSO sub-groups live with their handlers.
func (h *AuthHandler) registerRoutes(r chi.Router) {
	r.Route("/api/auth", func(r chi.Router) {
		r.Post("/register", h.handleAuthRegister)
		r.Post("/login", h.handleAuthLogin)
		r.Post("/refresh", h.handleAuthRefresh)
		r.Post("/forgot-password", h.handleAuthForgotPassword)
		r.Post("/reset-password", h.handleAuthResetPassword)
		r.Post("/verify-email", h.handleAuthVerifyEmail)
		r.Post("/resend-verification", h.handleAuthResendVerification)
		r.Get("/me", h.handleAuthMe)
		r.Put("/profile", h.handleAuthUpdateProfile)
		r.Post("/logout", h.handleAuthLogout)
		r.Post("/change-password", h.handleAuthChangePassword)
		r.Get("/sessions", h.handleAuthSessions)
		r.Delete("/sessions/{id}", h.handleAuthSessionRevoke)
		// Self-service account erasure (GDPR) + data-subject export.
		r.Delete("/account", h.handleAuthDeleteAccount)
		r.Get("/account/export", h.handleAuthExportAccount)
		h.registerTokenRoutes(r)
		h.registerSSORoutes(r)
	})
	r.Post("/api/ws-ticket", h.handleWSTicket)
}
