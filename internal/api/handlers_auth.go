package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type AuthHandler struct {
	tokenStore RefreshTokenStore
	backend    storageif.StorageBackend
	security   *SecurityConfig
}

func NewAuthHandler(tokenStore RefreshTokenStore, backend storageif.StorageBackend, security *SecurityConfig) *AuthHandler {
	return &AuthHandler{tokenStore: tokenStore, backend: backend, security: security}
}

func (h *AuthHandler) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("registration not available in local mode"), http.StatusForbidden)
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if !strings.Contains(req.Email, "@") || len(req.Password) < 8 {
		render.Error(w, fmt.Errorf("invalid email or password too short"), http.StatusBadRequest)
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
		Email:    req.Email,
		Password: hashed,
		Role:     auth.RoleMember,
	}

	user.Role = resolveRegistrationRole(r.Context(), h.backend)

	if err := h.backend.CreateUser(r.Context(), user); err != nil {
		render.Error(w, err, http.StatusConflict)
		return
	}

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
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	user, err := h.backend.LoadUserByEmail(r.Context(), req.Email)
	if err != nil {
		render.Error(w, fmt.Errorf("invalid credentials"), http.StatusUnauthorized)
		return
	}

	if !auth.CheckPasswordHash(req.Password, user.Password) {
		render.Error(w, fmt.Errorf("invalid credentials"), http.StatusUnauthorized)
		return
	}

	pair, err := h.security.AuthMgr.Issue(user.ID, user.Email, user.Role)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if h.tokenStore != nil {
		expiresAt := time.Now().Add(7 * 24 * time.Hour)
		if err := h.tokenStore.StoreRefreshToken(r.Context(), pair.RefreshID, user.ID, expiresAt); err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
	}

	logAudit(r.Context(), h.backend, r, AuditActionLogin, "user", user.ID, nil)
	render.JSON(w, pair)
}

func (h *AuthHandler) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
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
		valid, err := h.tokenStore.IsRefreshTokenValid(r.Context(), claims.ID)
		if err != nil || !valid {
			render.Error(w, fmt.Errorf("invalid or revoked refresh token"), http.StatusUnauthorized)
			return
		}
		_ = h.tokenStore.RevokeRefreshToken(r.Context(), claims.ID)
	}

	pair, err := h.security.AuthMgr.Issue(claims.UserID, claims.Email, claims.Role)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if h.tokenStore != nil {
		expiresAt := time.Now().Add(7 * 24 * time.Hour)
		_ = h.tokenStore.StoreRefreshToken(r.Context(), pair.RefreshID, claims.UserID, expiresAt)
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
	render.JSON(w, map[string]string{
		"id":    claims.UserID,
		"email": claims.Email,
		"role":  string(claims.Role),
	})
}

func (h *AuthHandler) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.RefreshToken != "" && h.tokenStore != nil {
		claims, err := h.security.AuthMgr.VerifyRefresh(req.RefreshToken)
		if err == nil {
			_ = h.tokenStore.RevokeRefreshToken(r.Context(), claims.ID)
		}
	}
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
		_ = h.tokenStore.RevokeUserRefreshTokens(r.Context(), user.ID)
	}

	render.JSON(w, map[string]string{"status": "ok"})
}

// resolveRegistrationRole returns RoleAdmin for the very first registered user
// (so there is always at least one admin), and RoleMember for everyone after.
func resolveRegistrationRole(ctx context.Context, backend storageif.StorageBackend) auth.Role {
	if users, _ := backend.ListUsers(ctx); len(users) == 0 {
		return auth.RoleAdmin
	}
	return auth.RoleMember
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
