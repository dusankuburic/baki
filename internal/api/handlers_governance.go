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
