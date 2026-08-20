package api

import (
	"net/http"
	"strings"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	"pad-core/models"
)

// DashboardHandler serves the welcome ("home") dashboard BFF endpoint.
type DashboardHandler struct {
	dashboard *service.DashboardService
	security  *SecurityConfig
}

func NewDashboardHandler(dashboard *service.DashboardService, security *SecurityConfig) *DashboardHandler {
	return &DashboardHandler{dashboard: dashboard, security: security}
}

// handleHome returns the assembled dashboard payload. BuildHome never hard-fails
// (it degrades to a sparse, availability-flagged payload), so this always 200s
// with valid JSON and the frontend renders empty states per-card as needed.
// @Summary      Aggregated dashboard data for the home view
// @Tags         dashboard
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      401 {object} object "Unauthorized"
// @Router       /api/dashboard/home [get]
func (h *DashboardHandler) handleHome(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	data := h.dashboard.BuildHome(r.Context(), userID)
	// BuildHome returns a SHARED cached pointer on a TTL-cache hit (cloud
	// mode) — mutating it in place races every concurrent caller's JSON
	// marshal (see dashboard.go's invariant note). Copy, then set the
	// per-request greeting on the copy. Safe: the slices/maps inside are
	// populated at build time and never mutated afterwards.
	cp := *data
	cp.Greeting = h.greeting(r)
	render.JSON(w, &cp)
}

func (h *DashboardHandler) greeting(r *http.Request) models.DashboardGreeting {
	if !h.security.JWTEnabled {
		return models.DashboardGreeting{UserDisplayName: h.security.LocalName}
	}
	name := ""
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		name = displayFromEmail(claims.Email)
	}
	if name == "" {
		name = "there"
	}
	return models.DashboardGreeting{UserDisplayName: name}
}

// displayFromEmail derives a friendly first-name-ish label from an email's local
// part (e.g. "ada.lovelace@x.com" → "ada.lovelace"). The frontend may format
// further; this just avoids showing a full address in the greeting.
func displayFromEmail(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}
