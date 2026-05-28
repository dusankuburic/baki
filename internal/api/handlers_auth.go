package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

func (rt *Router) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}

	if req.Email == "" || req.Password == "" {
		http.Error(w, "email and password required", http.StatusBadRequest)
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
		Role:     auth.RoleMember, // Default to member
	}

	// Check if this is the first user
	count, err := rt.app.StorageBackend().CountUsers(r.Context())
	if err == nil && count == 0 {
		user.Role = auth.RoleAdmin
	}

	// First user becomes admin
	if _, err := rt.app.StorageBackend().LoadUserByEmail(r.Context(), req.Email); err == nil {
		http.Error(w, "user already exists", http.StatusConflict)
		return
	}
	// We could count users to make the first one admin, but for now just create members
	// unless they are using local mode, in which case we don't use the DB.
	
	if err := rt.app.StorageBackend().SaveUser(r.Context(), user); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	pair, err := rt.authMgr.Issue(user.ID, user.Email, user.Role)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

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
	// Use the email and role embedded in the refresh token so they are preserved
	// across refreshes without trusting the client to supply them again.
	pair, err := rt.authMgr.Issue(rc.UserID, rc.Email, rc.Role)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]any{
		"accessToken":  pair.AccessToken,
		"refreshToken": pair.RefreshToken,
		"expiresAt":    pair.ExpiresAt,
	})
}

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

func (rt *Router) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	// Stateless JWT — invalidation is client-side; nothing to do server-side.
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleAuthChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
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
