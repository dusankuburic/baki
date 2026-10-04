package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
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
	// Locked accounts are an instance-wide count: only instance admins see
	// it. Security is a value field, so this touches the copy alone.
	cp.Security.Admin = h.isAdmin(r)
	if !cp.Security.Admin {
		cp.Security.LockedAccounts = 0
	}
	render.JSON(w, &cp)
}

// isAdmin reports whether the caller may see instance-wide counts: every
// caller in local mode (single user), instance admins under JWT.
func (h *DashboardHandler) isAdmin(r *http.Request) bool {
	if !h.security.JWTEnabled {
		return true
	}
	claims := auth.ClaimsFromContext(r.Context())
	return claims != nil && claims.Role == auth.RoleAdmin
}

// handleAnalytics serves the filterable analytics page. Cloud-only: the
// sections come from persisted analysis history, which local mode lacks.
// @Summary      Filterable analytics for the Analytics page
// @Description  Every section is computed for the same scope and window. Findings filters (severity, category, rule) map each flow's per-rule counts through the current rule catalog.
// @Tags         dashboard
// @Produce      json
// @Param        range     query string false "7d, 30d, 90d or custom (default 30d)"
// @Param        from      query string false "YYYY-MM-DD, inclusive (range=custom)"
// @Param        to        query string false "YYYY-MM-DD, inclusive (range=custom)"
// @Param        tz        query string false "IANA time zone for day bucketing (default UTC)"
// @Param        scope     query string false "personal, org or all (default all)"
// @Param        orgId     query string false "organisation id (scope=org)"
// @Param        flows     query string false "comma-separated flow ids (max 50)"
// @Param        severity  query string false "comma-separated: error, warning, info"
// @Param        category  query string false "comma-separated categories (max 20)"
// @Param        rule      query string false "comma-separated rule ids (max 50)"
// @Param        compare   query string false "1 (default) or 0: include the previous period"
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      403 {object} object "Forbidden"
// @Failure      501 {object} object "Not Implemented (local mode)"
// @Router       /api/dashboard/analytics [get]
func (h *DashboardHandler) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	if !h.dashboard.CloudMode() {
		render.Error(w, fmt.Errorf("analytics are available in cloud mode only"), http.StatusNotImplemented)
		return
	}
	q, err := parseAnalyticsQuery(r.URL.Query(), time.Now())
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	caller := service.AnalyticsCaller{}
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		caller.Email = claims.Email
		caller.IsAdmin = claims.Role == auth.RoleAdmin
	}
	data, err := h.dashboard.BuildAnalytics(r.Context(), h.security.CallerID(r), caller, q)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	// On a cache hit this is the shared cached pointer (see handleHome);
	// nothing per-request is written to it.
	render.JSON(w, data)
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

// registerRoutes wires the welcome-dashboard BFF routes.
func (h *DashboardHandler) registerRoutes(r chi.Router) {
	r.Get("/api/dashboard/home", h.handleHome)
	r.Get("/api/dashboard/analytics", h.handleAnalytics)
}
