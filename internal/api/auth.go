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

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

type AuthHandler struct {
	// sessions owns the refresh tokens; not Enabled without a token store.
	sessions *service.SessionService
	backend  storageif.StorageBackend
	security *SecurityConfig
	// ssoClient is nil and identity not Available unless OIDC SSO is configured
	// (cloud mode with PAD_SSO_* set and a Postgres backend).
	ssoClient SSOClient
	identity  *service.IdentityService
	// authSvc owns credential verification (bcrypt timing/lockout); see internal/service/auth.go.
	authSvc *service.AuthService
	// accounts owns account erasure and the data-subject export.
	accounts *service.AccountLifecycleService
	// apiTokens owns the machine API tokens.
	apiTokens *service.APITokenService
	audit     *audit.Recorder
	// bus announces the credentials revoked here (AccessRevoked), so the sockets they opened close at once; nil
	// announces nothing.
	bus *eventbus.Bus
}

func NewAuthHandler(sessions *service.SessionService, backend storageif.StorageBackend, security *SecurityConfig, ssoClient SSOClient, identity *service.IdentityService, authSvc *service.AuthService, accounts *service.AccountLifecycleService, apiTokens *service.APITokenService, recorder *audit.Recorder, bus *eventbus.Bus) *AuthHandler {
	return &AuthHandler{sessions: sessions, backend: backend, security: security, ssoClient: ssoClient, identity: identity, authSvc: authSvc, accounts: accounts, apiTokens: apiTokens, audit: recorder, bus: bus}
}

// accessRevoked announces that userID's credential jti was revoked, once the blacklist holds it: the sockets it
// opened re-check against the blacklist and must find it there.
func (h *AuthHandler) accessRevoked(r *http.Request, userID, jti string) {
	h.bus.Publish(r.Context(), events.AccessRevoked{UserID: userID, JTI: jti})
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// RememberMe shapes the refresh cookie's lifetime on the web build:
	// true → persistent (Expires = refresh-token TTL), false → a browser-session
	// cookie that dies with the browser, mirroring the old sessionStorage model.
	// The desktop build ignores it (it authenticates with the body token).
	Remember bool `json:"rememberMe"`
}

// refreshCookieName: HttpOnly keeps document.cookie/XSS out, SameSite=Lax blocks cross-site
// CSRF POSTs, Path=/api/auth confines it. The body field stays for compat.
const refreshCookieName = "pad_refresh"

// isHTTPS reports whether the request reached the server over TLS (directly, or via
// X-Forwarded-Proto from a peer configured in trustedProxies); the refresh cookie's Secure
// flag is set only for encrypted transport. The proxy gate matters in both directions: an
// arbitrary client must not force the flag on, nor suppress it on a TLS deployment.
func isHTTPS(r *http.Request, trustedProxies []string) bool {
	if r.TLS != nil {
		return true
	}
	if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" && middleware.PeerIsTrusted(r, trustedProxies) {
		return xfp == "https"
	}
	return false
}

// setRefreshCookie sets the HttpOnly refresh cookie. persistent=false omits
// Expires, making it a browser-session cookie — the web build maps the login
// form's "remember me" to this; true pins Expires to the token's TTL.
func setRefreshCookie(w http.ResponseWriter, r *http.Request, trustedProxies []string, token string, expires time.Time, persistent bool) {
	// #nosec G124 -- HttpOnly and SameSite are both set; only Secure is
	// conditional, and deliberately so. The desktop build serves plain HTTP on
	// loopback, where a hard Secure:true makes the browser drop the refresh
	// cookie and breaks login entirely. isHTTPS(r) honours the proxy headers in
	// cloud mode, where it resolves to true.
	c := http.Cookie{
		Name:     refreshCookieName,
		Value:    token,
		Path:     "/api/auth",
		HttpOnly: true,
		Secure:   isHTTPS(r, trustedProxies),
		SameSite: http.SameSiteLaxMode,
	}
	if persistent {
		c.Expires = expires
	}
	http.SetCookie(w, &c)
}

func clearRefreshCookie(w http.ResponseWriter, r *http.Request, trustedProxies []string) {
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
		Secure:   isHTTPS(r, trustedProxies),
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *AuthHandler) refreshTokenFromRequest(r *http.Request, bodyToken string) string {
	if c, err := r.Cookie(refreshCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return bodyToken
}

// decodeCredentials returns the normalized email, raw password and the
// remember-me flag, with two errors for caller policy: err is a malformed body
// (both 400); register answers emailErr 400, login ignores it — a malformed
// email yields 401 "invalid credentials" without revealing rules.
func decodeCredentials(r *http.Request) (email, password string, remember bool, emailErr error, err error) {
	var c credentials
	if err = json.NewDecoder(r.Body).Decode(&c); err != nil {
		return "", "", false, nil, err
	}
	email, emailErr = validateEmail(c.Email)
	return email, c.Password, c.Remember, emailErr, nil
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
	recordAuthOp("register")
	email, password, remember, emailErr, err := decodeCredentials(r)
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

	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionRegister, ResourceType: "user", ResourceID: user.ID})
	userJSON := authUserJSON(user)
	if user.EmailVerified {
		// No verification round trip pending (bootstrap admin is created verified): issue the session directly.
		pair, ok := h.issueSession(w, r, user.ID, user.Email, user.Role, remember)
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

// issueSession signs the user in: it records them as the request's actor, mints a token pair, persists the
// refresh token, sets the HttpOnly cookie (persistent per the caller's remember-me flag) and records the
// login audit; on failure it has written the error and returns ok=false.
func (h *AuthHandler) issueSession(w http.ResponseWriter, r *http.Request, userID, email string, role auth.Role, remember bool) (*auth.TokenPair, bool) {
	recordUser(r, userID, email, role)
	pair, err := h.security.AuthMgr.Issue(userID, email, role)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return nil, false
	}

	if h.sessions.Enabled() {
		ua, ip := r.UserAgent(), middleware.ClientIP(r, h.security.TrustedProxies)
		if err := h.sessions.Record(r.Context(), pair.RefreshID, userID, pair.RefreshExpiresAt, ua, ip); err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return nil, false
		}
	}

	// Set the HttpOnly refresh cookie; callers still return the body field for backward compatibility.
	setRefreshCookie(w, r, h.security.TrustedProxies, pair.RefreshToken, pair.RefreshExpiresAt, remember)

	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionLogin, ResourceType: "user", ResourceID: userID})
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
	recordAuthOp("login")
	// Login ignores emailErr and normalizes only (see decodeCredentials): a malformed email returns 401.
	email, password, remember, _, err := decodeCredentials(r)
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
		h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionLoginFailure, ResourceType: "email", ResourceID: email, Meta: map[string]string{"reason": "user_not_found"}})
		render.Error(w, fmt.Errorf("invalid credentials"), http.StatusUnauthorized)
		return
	case service.LoginAccountLocked:
		h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionLoginFailure, ResourceType: "user", ResourceID: result.User.ID, Meta: map[string]string{"reason": "locked"}})
		render.Error(w, fmt.Errorf("account temporarily locked; please try again later"), http.StatusForbidden)
		return
	case service.LoginInvalidPassword:
		if result.AccountJustLocked {
			h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionAccountLock, ResourceType: "user", ResourceID: result.User.ID})
		}
		h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionLoginFailure, ResourceType: "user", ResourceID: result.User.ID, Meta: map[string]string{"reason": "invalid_password"}})
		render.Error(w, fmt.Errorf("invalid credentials"), http.StatusUnauthorized)
		return
	case service.LoginEmailNotVerified:
		h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionLoginFailure, ResourceType: "user", ResourceID: result.User.ID, Meta: map[string]string{"reason": "email_not_verified"}})
		// Machine-readable code so the login page can offer "resend the link".
		render.ErrorWithCode(w, fmt.Errorf("email not verified — check your inbox for a verification link"), http.StatusForbidden, "email_not_verified")
		return
	}
	user := result.User

	pair, ok := h.issueSession(w, r, user.ID, user.Email, user.Role, remember)
	if !ok {
		return
	}
	// The client validates against LoginResponseSchema, which requires a `user` object — tokens alone failed validation.
	render.JSON(w, map[string]any{
		"user":             authUserJSON(user),
		"accessToken":      pair.AccessToken,
		"refreshToken":     pair.RefreshToken,
		"expiresAt":        pair.ExpiresAt,
		"refreshId":        pair.RefreshID,
		"refreshExpiresAt": pair.RefreshExpiresAt,
	})
}

// authUserJSON is the user object of the login and register responses: the claims' core plus the profile fields,
// so a client that just signed in can render the user without a /me round trip.
func authUserJSON(user *storageif.User) map[string]any {
	return map[string]any{
		"id":          user.ID,
		"email":       user.Email,
		"role":        user.Role,
		"displayName": user.DisplayName,
		"avatarUrl":   user.AvatarURL,
	}
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
	recordAuthOp("refresh")
	var req struct {
		RefreshToken string `json:"refreshToken"`
		// RememberMe reshapes the rotated cookie (persistent vs browser-session)
		// for cookie-authenticated web clients; body-token clients may send it
		// too, but their cookie is unused.
		RememberMe bool `json:"rememberMe"`
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

	if !h.sessions.Enabled() {
		// In JWT mode a nil token store is a misconfiguration: the replay and rotation safeguards all
		// depend on it — fail closed, else a stolen refresh token replays indefinitely.
		if h.security.JWTEnabled {
			logger.FromContext(r.Context()).Error("refresh requested in JWT mode without a token store (misconfiguration); rejecting",
				"user_id", claims.UserID, "token_id", claims.ID)
			render.Error(w, fmt.Errorf("session management unavailable — please contact an administrator"), http.StatusUnauthorized)
			return
		}
	} else if err := h.sessions.Rotate(r.Context(), claims.UserID, claims.ID); err != nil {
		// Rotate is an atomic verify-and-revoke: of two concurrent requests with the same jti only one succeeds.
		if errors.Is(err, service.ErrSessionReplayed) || errors.Is(err, service.ErrSessionUserMismatch) {
			render.Error(w, fmt.Errorf("session compromised — please log in again"), http.StatusUnauthorized)
			return
		}
		render.Error(w, fmt.Errorf("failed to process token refresh"), http.StatusInternalServerError)
		return
	}

	// Reload the user so the new token reflects the CURRENT role and email, not the refresh-token claims.
	issueRole := claims.Role
	issueEmail := claims.Email
	if h.backend != nil {
		user, err := h.authSvc.User(r.Context(), claims.UserID)
		if err != nil {
			// A1.2: fail closed — a deleted user must not refresh from stale claims.
			logger.FromContext(r.Context()).Warn("refresh for deleted/missing user", "userID", claims.UserID, "error", err)
			render.Error(w, fmt.Errorf("account not found — please log in again"), http.StatusUnauthorized)
			return
		}
		// A1.1: Locked accounts must not be able to refresh their way back in.
		if user.LockedUntil != nil && time.Now().UTC().Before(*user.LockedUntil) {
			logger.FromContext(r.Context()).Warn("refresh blocked for locked user", "userID", claims.UserID)
			render.Error(w, fmt.Errorf("account is temporarily locked"), http.StatusUnauthorized)
			return
		}
		issueRole = user.Role
		issueEmail = user.Email
	}

	recordUser(r, claims.UserID, issueEmail, issueRole)
	pair, err := h.security.AuthMgr.Issue(claims.UserID, issueEmail, issueRole)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if h.sessions.Enabled() {
		ua, ip := r.UserAgent(), middleware.ClientIP(r, h.security.TrustedProxies)
		if err := h.sessions.Record(r.Context(), pair.RefreshID, claims.UserID, pair.RefreshExpiresAt, ua, ip); err != nil {
			logger.FromContext(r.Context()).Error("failed to store new refresh token", "error", err, "userID", claims.UserID)
			render.Error(w, fmt.Errorf("failed to store token"), http.StatusInternalServerError)
			return
		}
	}

	// Rotate the HttpOnly cookie to the newly-issued refresh token, keeping the
	// session shape the client asked for (persistent vs browser-session).
	setRefreshCookie(w, r, h.security.TrustedProxies, pair.RefreshToken, pair.RefreshExpiresAt, req.RememberMe)

	render.JSON(w, pair)
}

// MeResponse is the caller's identity as GET /api/auth/me returns it: the claims' core, the profile the store
// holds (display name, avatar, the account's stamps) and the settings document's profile section. The local-mode
// branch carries no profile: the local user has no account row and no user settings.
type MeResponse struct {
	ID            string                     `json:"id"`
	Email         string                     `json:"email"`
	Role          string                     `json:"role"`
	DisplayName   string                     `json:"displayName"`
	AvatarURL     string                     `json:"avatarUrl"`
	CreatedAt     string                     `json:"createdAt,omitempty"`
	EmailVerified bool                       `json:"emailVerified,omitempty"`
	Profile       *storageif.ProfileSettings `json:"profile,omitempty"`
}

// @Summary      Get current user info
// @Description  Returns information about the currently authenticated user: the claims' core, the stored profile (display name, avatar URL, account stamps) and, in cloud mode, the settings document's profile section (bio, job title, timezone). The web client reads that section from its settings store; it is here for other clients (the CLI, scripts) so they need no second call.
// @Tags         auth
// @Produce      json
// @Success      200 {object} MeResponse "OK"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Router       /api/auth/me [get]
func (h *AuthHandler) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		if !h.security.JWTEnabled {
			render.JSON(w, MeResponse{
				ID:    h.security.LocalUserID,
				Email: h.security.LocalName,
				Role:  string(auth.RoleAdmin),
			})
			return
		}
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	resp := MeResponse{
		ID:    claims.UserID,
		Email: claims.Email,
		Role:  string(claims.Role),
	}
	if h.backend != nil {
		if user, err := h.authSvc.User(r.Context(), claims.UserID); err == nil {
			resp.DisplayName = user.DisplayName
			resp.AvatarURL = user.AvatarURL
			resp.CreatedAt = user.CreatedAt.UTC().Format(time.RFC3339)
			resp.EmailVerified = user.EmailVerified
		}
		// The identity-card fields ride the user's own settings document; a read failure keeps the response
		// without them rather than failing an endpoint whose core (the claims) is already known.
		if profile, err := h.authSvc.ProfileFields(r.Context(), claims.UserID); err == nil {
			resp.Profile = &profile
		}
	}
	render.JSON(w, resp)
}

// @Summary      Update profile
// @Description  handleAuthUpdateProfile lets a user set their own display name and avatar URL. Both fields are optional and may be cleared by sending an empty string. The avatar URL is an http(s) URL, or the caller's own avatar route (/api/users/{id}/avatar) to keep an uploaded picture while changing the name; any other value, an empty one included, replaces the uploaded picture and deletes the stored image. The route without an upload behind it is a 400.
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
	// The caller's own serving route is what an upload left in the profile; saving the name must be able to send it back.
	if req.AvatarURL != "" && req.AvatarURL != userAvatarRoute(claims.UserID) {
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

	if err := h.authSvc.UpdateProfile(r.Context(), claims.UserID, req.DisplayName, req.AvatarURL, userAvatarRoute(claims.UserID)); err != nil {
		if errors.Is(err, service.ErrAvatarImage) {
			render.Error(w, err, http.StatusBadRequest)
			return
		}
		if errors.Is(err, storageif.ErrNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionProfileUpdate, ResourceType: "user", ResourceID: claims.UserID})
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
	recordAuthOp("logout")
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.FromContext(r.Context()).Warn("logout: failed to decode body, proceeding without refresh token revocation", "error", err)
	}

	refreshTok := h.refreshTokenFromRequest(r, req.RefreshToken)
	if refreshTok != "" && h.sessions.Enabled() {
		claims, err := h.security.AuthMgr.VerifyRefresh(refreshTok)
		if err == nil {
			if err := h.sessions.Revoke(r.Context(), claims.ID); err != nil {
				logger.FromContext(r.Context()).Error("failed to revoke refresh token during logout", "error", err, "tokenID", claims.ID)
			}
		}
	}

	clearRefreshCookie(w, r, h.security.TrustedProxies)

	if tokenStr := auth.ExtractToken(r); tokenStr != "" {
		if claims, err := h.security.AuthMgr.VerifyIgnoreExpiry(tokenStr); err == nil && claims.ID != "" {
			h.security.AuthMgr.Revoke(claims)
			h.accessRevoked(r, claims.UserID, claims.ID)
		}
	}

	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionLogout})
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

// Mount registers /api/auth (register/login/refresh, profile, password,
// sessions, account, tokens, SSO) and the WebSocket ticket route. Password,
// account, token and SSO sub-groups live with their handlers. Scoped tokens
// never manage credentials or identity: a read-scoped token minting itself a
// full-access one would be an escalation.
func (h *AuthHandler) Mount(rb *route.Builder) {
	rb.Route("/api/auth", func(rb *route.Builder) {
		// Sign-in and recovery come before any session. They take credentials or
		// one-time tokens (bcrypt, email), so they ride the tighter auth bucket.
		rb.Group(func(rb *route.Builder) {
			rb.Post("/register", h.handleAuthRegister)
			rb.Post("/login", h.handleAuthLogin)
			rb.Post("/refresh", h.handleAuthRefresh)
			rb.Post("/forgot-password", h.handleAuthForgotPassword)
			rb.Post("/reset-password", h.handleAuthResetPassword)
			rb.Post("/verify-email", h.handleAuthVerifyEmail)
			rb.Post("/resend-verification", h.handleAuthResendVerification)
		}, route.Public(), route.RateLimit(route.RateAuth))
		// Identity self-check: CI validates its token with it, and it reveals
		// nothing beyond the caller.
		rb.Get("/me", h.handleAuthMe, route.Scopes(auth.ScopeRead))
		rb.Put("/profile", h.handleAuthUpdateProfile)
		h.mountAvatarProfileRoutes(rb)
		// Works on an expired session: it revokes whatever tokens the request carries.
		rb.Post("/logout", h.handleAuthLogout, route.Public())
		rb.Post("/change-password", h.handleAuthChangePassword, route.RateLimit(route.RateAuth))
		rb.Get("/sessions", h.handleAuthSessions)
		rb.Delete("/sessions/{id}", h.handleAuthSessionRevoke)
		// Self-service account erasure (GDPR) + data-subject export.
		rb.Group(func(rb *route.Builder) {
			rb.Delete("/account", h.handleAuthDeleteAccount)
			rb.Get("/account/export", h.handleAuthExportAccount)
		}, route.Needs(capability.ServerStore))
		h.registerTokenRoutes(rb)
		h.registerSSORoutes(rb)
	}, route.DenyTokens())
	h.mountAvatarServeRoute(rb)
	// The ticket is the entry to live updates: read-level.
	rb.Post("/api/ws-ticket", h.handleWSTicket, route.Scopes(auth.ScopeRead))
}
