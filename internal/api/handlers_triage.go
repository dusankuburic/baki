package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
)

// Persistent, team-shared finding triage & baselines. These endpoints back the
// governance workflow on top of static analysis: a finding's status/justification
// is pinned to its stable key (models.Finding.Key) so it survives re-analysis,
// and a baseline snapshots the accepted set of keys for ratcheting.
//
// Triage is inherently a multi-user/cloud feature, so it requires a storage
// backend; in desktop/in-memory mode (backend == nil) these return 503, mirroring
// the admin/dashboard cloud-only endpoints.

// validTriageStatuses is the allowed lifecycle for a finding's triage state.
var validTriageStatuses = map[string]bool{
	"open":         true,
	"acknowledged": true,
	"in_progress":  true,
	"resolved":     true,
	"suppressed":   true,
}

func (h *AnalysisHandler) triageAvailable(w http.ResponseWriter) bool {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("finding triage requires a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *AnalysisHandler) handleListFindingStatuses(w http.ResponseWriter, r *http.Request) {
	if !h.triageAvailable(w) {
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "viewer"); err != nil {
		render.Error(w, err, 0)
		return
	}

	statuses, err := h.backend.ListFindingStatuses(r.Context(), req.FlowID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if statuses == nil {
		statuses = []*storageif.FindingStatus{}
	}
	render.JSON(w, statuses)
}

func (h *AnalysisHandler) handleSetFindingStatus(w http.ResponseWriter, r *http.Request) {
	if !h.triageAvailable(w) {
		return
	}
	var req struct {
		FlowID        string `json:"flowId"`
		FindingKey    string `json:"findingKey"`
		RuleID        string `json:"ruleId"`
		Status        string `json:"status"`
		Justification string `json:"justification"`
		AssigneeID    string `json:"assigneeId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if req.FindingKey == "" {
		render.Error(w, fmt.Errorf("findingKey is required"), http.StatusBadRequest)
		return
	}
	if !validTriageStatuses[req.Status] {
		render.Error(w, fmt.Errorf("invalid status %q", req.Status), http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}

	st := &storageif.FindingStatus{
		FlowID:        req.FlowID,
		FindingKey:    req.FindingKey,
		RuleID:        req.RuleID,
		Status:        req.Status,
		Justification: req.Justification,
		AssigneeID:    req.AssigneeID,
		UpdatedBy:     userID,
	}
	if err := h.backend.SetFindingStatus(r.Context(), st); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFindingTriage, "flow", req.FlowID,
		map[string]string{"findingKey": req.FindingKey, "status": req.Status})
	render.JSON(w, st)
}

// handleBatchSetFindingStatus applies the same lifecycle update to many findings
// of one flow in a single request, collapsing the N round-trips the client would
// otherwise make (e.g. bulk-suppressing a rule's findings). Authorization is
// checked once for the flow; all items are validated up-front so a bad item
// rejects the whole batch before any write.
func (h *AnalysisHandler) handleBatchSetFindingStatus(w http.ResponseWriter, r *http.Request) {
	if !h.triageAvailable(w) {
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
		Items  []struct {
			FindingKey    string `json:"findingKey"`
			RuleID        string `json:"ruleId"`
			Status        string `json:"status"`
			Justification string `json:"justification"`
			AssigneeID    string `json:"assigneeId"`
		} `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	const maxBatch = 1000
	if len(req.Items) == 0 {
		render.Error(w, fmt.Errorf("items is required"), http.StatusBadRequest)
		return
	}
	if len(req.Items) > maxBatch {
		render.Error(w, fmt.Errorf("too many items (max %d)", maxBatch), http.StatusBadRequest)
		return
	}
	for i, it := range req.Items {
		if it.FindingKey == "" {
			render.Error(w, fmt.Errorf("items[%d]: findingKey is required", i), http.StatusBadRequest)
			return
		}
		if !validTriageStatuses[it.Status] {
			render.Error(w, fmt.Errorf("items[%d]: invalid status %q", i, it.Status), http.StatusBadRequest)
			return
		}
	}

	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}

	for _, it := range req.Items {
		st := &storageif.FindingStatus{
			FlowID:        req.FlowID,
			FindingKey:    it.FindingKey,
			RuleID:        it.RuleID,
			Status:        it.Status,
			Justification: it.Justification,
			AssigneeID:    it.AssigneeID,
			UpdatedBy:     userID,
		}
		if err := h.backend.SetFindingStatus(r.Context(), st); err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFindingTriage, "flow", req.FlowID,
		map[string]string{"batch": fmt.Sprintf("%d", len(req.Items)), "status": req.Items[0].Status})
	render.JSON(w, map[string]int{"updated": len(req.Items)})
}

func (h *AnalysisHandler) handleClearFindingStatus(w http.ResponseWriter, r *http.Request) {
	if !h.triageAvailable(w) {
		return
	}
	var req struct {
		FlowID     string `json:"flowId"`
		FindingKey string `json:"findingKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if req.FindingKey == "" {
		render.Error(w, fmt.Errorf("findingKey is required"), http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}

	if err := h.backend.DeleteFindingStatus(r.Context(), req.FlowID, req.FindingKey); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *AnalysisHandler) handleGetBaseline(w http.ResponseWriter, r *http.Request) {
	if !h.triageAvailable(w) {
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "viewer"); err != nil {
		render.Error(w, err, 0)
		return
	}

	bl, err := h.backend.GetFlowBaseline(r.Context(), req.FlowID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, bl) // null when no baseline is set
}

func (h *AnalysisHandler) handleSetBaseline(w http.ResponseWriter, r *http.Request) {
	if !h.triageAvailable(w) {
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "editor")
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	report, err := h.analysisSvc.AnalyzeFlow(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	// Snapshot the current findings' stable keys as the accepted baseline.
	seen := make(map[string]bool, len(report.Findings))
	keys := make([]string, 0, len(report.Findings))
	for _, f := range report.Findings {
		k := f.Key()
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}

	bl := &storageif.FlowBaseline{FlowID: req.FlowID, Keys: keys, CreatedBy: userID}
	if err := h.backend.SetFlowBaseline(r.Context(), bl); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionBaselineSet, "flow", req.FlowID,
		map[string]string{"keys": fmt.Sprintf("%d", len(keys))})
	render.JSON(w, bl)
}

func (h *AnalysisHandler) handleClearBaseline(w http.ResponseWriter, r *http.Request) {
	if !h.triageAvailable(w) {
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}

	if err := h.backend.ClearFlowBaseline(r.Context(), req.FlowID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionBaselineClear, "flow", req.FlowID, nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleBaselineDrift analyzes the flow and reports findings introduced since the
// accepted baseline (the "new since baseline" ratchet). With no baseline set,
// every finding is reported as new (HasBaseline=false).
func (h *AnalysisHandler) handleBaselineDrift(w http.ResponseWriter, r *http.Request) {
	if !h.triageAvailable(w) {
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "viewer")
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	report, err := h.analysisSvc.AnalyzeFlow(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	bl, err := h.backend.GetFlowBaseline(r.Context(), req.FlowID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	// nil keys ⇒ no baseline (everything new); non-nil (even empty) ⇒ baseline exists.
	var keys []string
	if bl != nil {
		keys = bl.Keys
		if keys == nil {
			keys = []string{}
		}
	}

	render.JSON(w, analyzer.ComputeDrift(req.FlowID, report, keys))
}
