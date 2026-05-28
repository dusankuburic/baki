package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/storage/interfaces"
)

const minPasswordLength = 8

// recordRefresh persists a newly-issued refresh token in the rotation store
// (cloud mode only). Best-effort: a store failure is logged, and because the
// token then won't be considered valid, the next refresh simply forces re-login.
func (rt *Router) recordRefresh(ctx context.Context, pair *auth.TokenPair, userID string) {
	if rt.tokenStore == nil || pair.RefreshID == "" {
		return
	}
	if err := rt.tokenStore.StoreRefreshToken(ctx, pair.RefreshID, userID, pair.RefreshExpiresAt); err != nil {
		logger.Error("failed to store refresh token", "error", err)
	}
}

// validateCredentials checks that the email is well-formed and the password
// meets the minimum length. Returns a human-readable message on failure.
func validateCredentials(email, password string) (string, bool) {
	if _, err := mail.ParseAddress(email); err != nil {
		return "invalid email address", false
	}
	if len(password) < minPasswordLength {
		return "password must be at least 8 characters", false
	}
	return "", true
}

// @Summary Register a new user
// @Description Creates a new user account with email and password.
// @Tags auth
// @Accept json
// @Produce json
// @Param request body object{email=string,password=string} true "Registration Request"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Failure 409 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/auth/register [post]
func (rt *Router) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}

	if msg, ok := validateCredentials(req.Email, req.Password); !ok {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// Reject duplicate emails before doing any expensive hashing.
	if _, err := rt.app.StorageBackend().LoadUserByEmail(r.Context(), req.Email); err == nil {
		http.Error(w, "user already exists", http.StatusConflict)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	user := &interfaces.User{
		ID:       uuid.New().String(),
		Email:    req.Email,
		Password: string(hash),
		Role:     auth.RoleMember,
	}

	// The very first registered user is promoted to admin so the instance has an
	// initial administrator. SaveUser enforces email uniqueness, guarding against
	// a concurrent duplicate registration.
	if count, err := rt.app.StorageBackend().CountUsers(r.Context()); err == nil && count == 0 {
		user.Role = auth.RoleAdmin
	}

	if err := rt.app.StorageBackend().SaveUser(r.Context(), user); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	pair, err := rt.authMgr.Issue(user.ID, user.Email, user.Role)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.recordRefresh(r.Context(), pair, user.ID)

	rt.sendJSON(w, map[string]any{
		"accessToken":  pair.AccessToken,
		"refreshToken": pair.RefreshToken,
		"expiresAt":    pair.ExpiresAt,
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		},
	})
}

// @Summary Login user
// @Description Authenticates a user and returns access and refresh tokens.
// @Tags auth
// @Accept json
// @Produce json
// @Param request body object{email=string,password=string} true "Login Request"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/auth/login [post]
func (rt *Router) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}

	var userID, email string
	var role auth.Role

	if rt.jwtEnabled {
		// Cloud mode: use real credential verification
		user, err := rt.app.StorageBackend().LoadUserByEmail(r.Context(), req.Email)
		if err != nil {
			http.Error(w, "Invalid credentials", http.StatusUnauthorized)
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
			http.Error(w, "Invalid credentials", http.StatusUnauthorized)
			return
		}
		userID = user.ID
		email = user.Email
		role = user.Role
	} else {
		// Local/Tauri mode: the pre-shared token already guards the endpoint.
		// Ignore password and use defaults.
		userID = "local"
		email = "local@localhost"
		role = auth.RoleAdmin
	}

	pair, err := rt.authMgr.Issue(userID, email, role)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.recordRefresh(r.Context(), pair, userID)
	rt.sendJSON(w, map[string]any{
		"accessToken":  pair.AccessToken,
		"refreshToken": pair.RefreshToken,
		"expiresAt":    pair.ExpiresAt,
		"user": map[string]any{
			"id":    userID,
			"email": email,
			"role":  role,
		},
	})
}

// @Summary Refresh access token
// @Description Returns a new access token using a valid refresh token.
// @Tags auth
// @Accept json
// @Produce json
// @Param request body object{refreshToken=string} true "Refresh Request"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/auth/refresh [post]
func (rt *Router) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	rc, err := rt.authMgr.VerifyRefresh(req.RefreshToken)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Rotation (cloud mode): the presented token must still be valid in the
	// store; we then revoke it so it cannot be replayed. Each refresh hands out
	// a brand-new refresh token, limiting the blast radius of a leaked token.
	if rt.tokenStore != nil {
		if rc.ID == "" {
			// Pre-rotation token (no jti) — force a fresh login.
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		valid, err := rt.tokenStore.IsRefreshTokenValid(r.Context(), rc.ID)
		if err != nil {
			rt.sendError(w, err, http.StatusInternalServerError)
			return
		}
		if !valid {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if err := rt.tokenStore.RevokeRefreshToken(r.Context(), rc.ID); err != nil {
			rt.sendError(w, err, http.StatusInternalServerError)
			return
		}
	}

	// Use the email and role embedded in the refresh token so they are preserved
	// across refreshes without trusting the client to supply them again.
	pair, err := rt.authMgr.Issue(rc.UserID, rc.Email, rc.Role)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.recordRefresh(r.Context(), pair, rc.UserID)
	rt.sendJSON(w, map[string]any{
		"accessToken":  pair.AccessToken,
		"refreshToken": pair.RefreshToken,
		"expiresAt":    pair.ExpiresAt,
	})
}

// @Summary Get current user info
// @Description Returns information about the currently authenticated user.
// @Tags auth
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 401 {object} map[string]string
// @Router /api/auth/me [get]
func (rt *Router) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	raw := r.Header.Get("Authorization")
	tokenStr := strings.TrimPrefix(raw, "Bearer ")
	if tokenStr == "" || tokenStr == raw {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	claims, err := rt.authMgr.Verify(tokenStr)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	rt.sendJSON(w, map[string]any{
		"userId": claims.UserID,
		"email":  claims.Email,
		"role":   claims.Role,
	})
}

// @Summary Logout user
// @Description Logs out the current user. In stateless JWT mode, this is primarily a client-side operation.
// @Tags auth
// @Produce json
// @Success 200 {object} map[string]string
// @Router /api/auth/logout [post]
func (rt *Router) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	// In cloud mode, revoke the user's refresh tokens so they can't be replayed
	// after logout. The access token is short-lived and expires on its own.
	if rt.tokenStore != nil {
		if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
			if err := rt.tokenStore.RevokeUserRefreshTokens(r.Context(), claims.UserID); err != nil {
				logger.Error("logout: failed to revoke refresh tokens", "error", err)
			}
		}
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Issue a WebSocket connect ticket
// @Description Returns a short-lived, single-use ticket the client exchanges for a WebSocket connection, keeping the access token out of the WS URL.
// @Tags auth
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 401 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/ws-ticket [post]
func (rt *Router) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	// Identity resolution mirrors the WS handler's previous behaviour: use the
	// authenticated JWT claims in cloud mode, fall back to the local identity in
	// desktop mode (where the request is gated by the pre-shared static token).
	userID, email := rt.localUserID, rt.localName
	role := auth.RoleAdmin
	if rt.jwtEnabled {
		claims := auth.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		userID, email, role = claims.UserID, claims.Email, claims.Role
	}

	ticket, expiresAt, err := rt.authMgr.IssueWSTicket(userID, email, role)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]any{
		"ticket":    ticket,
		"expiresAt": expiresAt,
	})
}

// @Summary Change password
// @Description Changes the password for the currently authenticated user.
// @Tags auth
// @Accept json
// @Produce json
// @Param request body object{currentPassword=string,newPassword=string} true "Change Password Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/auth/change-password [post]
func (rt *Router) handleAuthChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}

	if len(req.NewPassword) < minPasswordLength {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	user, err := rt.app.StorageBackend().LoadUserByID(r.Context(), claims.UserID)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.CurrentPassword)); err != nil {
		http.Error(w, "invalid current password", http.StatusUnauthorized)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	user.Password = string(hash)
	if err := rt.app.StorageBackend().SaveUser(r.Context(), user); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	rt.sendJSON(w, map[string]string{"status": "ok"})
}
