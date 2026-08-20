package api

import (
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// In-app governance alerts inbox. The continuous-governance scanner persists
// alerts here (drift / health regression) as it detects them; these endpoints
// expose the inbox to the notifications bell. Alerts are team-shared and
// RLS-scoped to flows the caller can see, so no per-flow authz gate is needed
// (the storage query is filtered by RLS to the caller's visible flows).
//
// Like triage, this is a cloud/backend feature; in pure-local mode (backend ==
// nil) these return 503.

// governanceAvailable reports whether the alerts inbox is backed by a store.
func (h *AnalysisHandler) governanceAvailable(w http.ResponseWriter) bool {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("governance alerts require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return false
	}
	return true
}

// handleListGovernanceAlerts — GET /api/governance/alerts
// Returns visible alerts newest-first. ?includeDismissed=true surfaces dismissed
// alerts too; ?limit/&offset paginate (default/cap via clampListLimit).
// @Summary      List governance alerts
// @Description  handleListGovernanceAlerts — GET /api/governance/alerts Returns visible alerts newest-first. ?includeDismissed=true surfaces dismissed alerts too; ?limit/&offset paginate (default/cap via clampListLimit).
// @Tags         governance
// @Produce      json
// @Success      200 {object} map[string]interface{} "Alerts"
// @Router       /api/governance/alerts [get]
func (h *AnalysisHandler) handleListGovernanceAlerts(w http.ResponseWriter, r *http.Request) {
	if !h.governanceAvailable(w) {
		return
	}
	limit, ok := clampListLimit(w, r.URL.Query().Get("limit"))
	if !ok {
		return
	}
	offset, ok := clampListOffset(w, r.URL.Query().Get("offset"))
	if !ok {
		return
	}
	filter := storageif.GovernanceAlertFilter{
		Limit:            limit,
		Offset:           offset,
		IncludeDismissed: r.URL.Query().Get("includeDismissed") == "true",
	}
	alerts, err := h.backend.ListGovernanceAlerts(r.Context(), filter)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if alerts == nil {
		alerts = []*storageif.GovernanceAlert{}
	}
	render.JSON(w, alerts)
}

// handleUnreadGovernanceAlertCount — GET /api/governance/alerts/unread-count
// Returns {"count": N} for the bell badge (lighter than listing).
// @Summary      Unread alert count
// @Description  handleUnreadGovernanceAlertCount — GET /api/governance/alerts/unread-count Returns {"count": N} for the bell badge (lighter than listing).
// @Tags         governance
// @Produce      json
// @Success      200 {object} map[string]interface{} "Count"
// @Router       /api/governance/alerts/unread-count [get]
func (h *AnalysisHandler) handleUnreadGovernanceAlertCount(w http.ResponseWriter, r *http.Request) {
	if !h.governanceAvailable(w) {
		return
	}
	n, err := h.backend.UnreadGovernanceAlertCount(r.Context())
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]int{"count": n})
}

// handleMarkGovernanceAlertRead — POST /api/governance/alerts/read  {id}
// @Summary      Mark alert read
// @Description  handleMarkGovernanceAlertRead — POST /api/governance/alerts/read {id}
// @Tags         governance
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/governance/alerts/read [post]
func (h *AnalysisHandler) handleMarkGovernanceAlertRead(w http.ResponseWriter, r *http.Request) {
	if !h.governanceAvailable(w) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ID == "" {
		render.Error(w, fmt.Errorf("id is required"), http.StatusBadRequest)
		return
	}
	if err := h.backend.MarkGovernanceAlertRead(r.Context(), req.ID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleMarkAllGovernanceAlertsRead — POST /api/governance/alerts/read-all
// The "open the panel → clear the badge" action.
// @Summary      Mark all alerts read
// @Description  handleMarkAllGovernanceAlertsRead — POST /api/governance/alerts/read-all The "open the panel → clear the badge" action.
// @Tags         governance
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/governance/alerts/read-all [post]
func (h *AnalysisHandler) handleMarkAllGovernanceAlertsRead(w http.ResponseWriter, r *http.Request) {
	if !h.governanceAvailable(w) {
		return
	}
	if err := h.backend.MarkAllGovernanceAlertsRead(r.Context()); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleDismissGovernanceAlert — POST /api/governance/alerts/dismiss  {id}
// @Summary      Dismiss alert
// @Description  handleDismissGovernanceAlert — POST /api/governance/alerts/dismiss {id}
// @Tags         governance
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/governance/alerts/dismiss [post]
func (h *AnalysisHandler) handleDismissGovernanceAlert(w http.ResponseWriter, r *http.Request) {
	if !h.governanceAvailable(w) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ID == "" {
		render.Error(w, fmt.Errorf("id is required"), http.StatusBadRequest)
		return
	}
	if err := h.backend.DismissGovernanceAlert(r.Context(), req.ID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleClearGovernanceAlerts — DELETE /api/governance/alerts
// Permanently removes the caller's visible dismissed alerts.
// @Summary      Clear all alerts
// @Description  handleClearGovernanceAlerts — DELETE /api/governance/alerts Permanently removes the caller's visible dismissed alerts.
// @Tags         governance
// @Produce      json
// @Success      200 {object} map[string]interface{} "Cleared"
// @Router       /api/governance/alerts [delete]
func (h *AnalysisHandler) handleClearGovernanceAlerts(w http.ResponseWriter, r *http.Request) {
	if !h.governanceAvailable(w) {
		return
	}
	if err := h.backend.ClearGovernanceAlerts(r.Context()); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}
