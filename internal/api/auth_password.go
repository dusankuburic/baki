package api

import (
	"errors"
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	"pad-core/logger"
)

// @Summary      Resend email verification
// @Description  Re-sends the verification link for a registered, still-unverified address. Always returns 200 so the endpoint cannot be used to probe which emails exist.
// @Tags         auth
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/auth/resend-verification [post]
func (h *AuthHandler) handleAuthResendVerification(w http.ResponseWriter, r *http.Request) {
	if !h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("email verification not available in local mode"), http.StatusForbidden)
		return
	}
	metrics.RecordAuthOp("resend_verification")
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

	user, err := h.authSvc.ResendVerification(r.Context(), email)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if user != nil {
		logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionRegister, "user", user.ID, map[string]string{"step": "verification_resent"})
	}
	render.JSON(w, ok)
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

// handleAuthForgotPassword issues a reset token and emails its link; always
// 200 with the same body, so the endpoint cannot enumerate accounts.
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
