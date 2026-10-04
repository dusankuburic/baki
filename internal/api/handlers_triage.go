package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/logger"
)

// Persistent, team-shared finding triage & baselines. A finding's status/justification is pinned to
// its stable key (models.Finding.Key) so it survives re-analysis; a baseline snapshots the accepted
// set of keys for ratcheting. Requires a storage backend — in desktop mode these return 503.

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

// @Summary      List finding triage statuses for a flow
// @Tags         triage
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/triage/list [post]
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
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "viewer"); err != nil {
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

// @Summary      Set triage status for a finding
// @Tags         triage
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/triage/set [post]
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
	if !decodeBody(w, r, &req) {
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
	// Header, not the resolved document: the write needs the editor check and
	// the flow's name (for the assignment notice), never its content.
	flow, err := h.flowSvc.GetAuthorizedHeader(r.Context(), req.FlowID, userID, "editor")
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	// Fetch the prior assignee so we only notify on an actual assignment change
	// (not on every status tweak that re-sends the same assignee).
	priorAssignee := h.priorAssignee(r.Context(), req.FlowID, req.FindingKey)

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

	// Best-effort assignment notification: email the assignee only when the assignment actually
	// changed, detached so SMTP does not delay the response; the email service is nil-safe.
	if req.AssigneeID != "" && req.AssigneeID != priorAssignee {
		flowName := ""
		if flow != nil {
			flowName = flow.Name
		}
		// After the commit, and detached: run inline here the alert/email held a SECOND pooled
		// connection while the RLS transaction pinned the first (pool exhaustion), and announced
		// an assignment the transaction could still roll back. RegisterPostCommit defers the hook
		// until the middleware commits; h.alerts caps how many run and is drained on shutdown.
		actorEmail := claimsEmail(r.Context())
		storagedb.RegisterPostCommit(r.Context(), func() {
			fanOut := func() {
				h.fanOutAssignment(req.FlowID, req.FindingKey, req.RuleID, flowName, req.AssigneeID, userID, actorEmail)
			}
			if !h.alerts.Go(fanOut) {
				// Pool saturated or draining: the alert row must not be lost
				// (it never could be when the hook ran it inline).
				fanOut()
			}
		})
	}

	render.JSON(w, st)
}

// fanOutAssignment is the detached work of one assignment change: resolve the assigner's display
// name once, record the assignee's targeted in-app alert, send the assignment email from h.emails
// (SMTP never holds the alert back). Bounded by bestEffortTimeout.
func (h *AnalysisHandler) fanOutAssignment(flowID, findingKey, ruleID, flowName, assigneeID, assignerID, assignerEmail string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("finding-assignment fan-out panicked", "err", r)
		}
	}()
	if h.backend == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), bestEffortTimeout)
	defer cancel()
	assignerName := h.callerDisplayName(ctx, assignerID, assignerEmail)
	h.recordFindingAlert(ctx, &storageif.GovernanceAlert{
		ID:         fmt.Sprintf("%s|finding_assigned|%s|%s", flowID, findingKey, assigneeID),
		FlowID:     flowID,
		FlowName:   flowName,
		Type:       "finding_assigned",
		Title:      h.ruleDisplayName(ruleID),
		Message:    fmt.Sprintf("Assigned to you by %s", assignerName),
		Severity:   "info",
		TargetUser: assigneeID,
	})
	h.emails.Go(func() {
		ectx, ecancel := context.WithTimeout(context.Background(), bestEffortTimeout)
		defer ecancel()
		h.notifyFindingAssignment(ectx, assigneeID, assignerName, flowName, ruleID)
	})
}

// priorAssignee returns the finding's current assignee ("" on no state or lookup error — the caller
// then notifies, the safe direction). One primary-key row when the backend offers it, else a list.
func (h *AnalysisHandler) priorAssignee(ctx context.Context, flowID, findingKey string) string {
	if g, ok := h.backend.(storageif.FindingStatusGetter); ok {
		if st, err := g.GetFindingStatus(ctx, flowID, findingKey); err == nil && st != nil {
			return st.AssigneeID
		}
		return ""
	}
	if statuses, err := h.backend.ListFindingStatuses(ctx, flowID); err == nil {
		for _, ps := range statuses {
			if ps.FindingKey == findingKey {
				return ps.AssigneeID
			}
		}
	}
	return ""
}

// bestEffortTimeout bounds the detached best-effort work hanging off a triage or comment write: it
// runs on context.Background() so a failure cannot poison the caller's RLS transaction, but
// Background never cancels — without a bound a slow DB keeps the goroutine (and its pooled
// connection) alive indefinitely, including through shutdown drain.
const bestEffortTimeout = 10 * time.Second

// recordFindingAlert persists a personal (targeted) in-app alert for the bell, best-effort and
// detached after the commit. The ID embeds the (flow, type, key, target) tuple — ON CONFLICT DO
// NOTHING makes retries idempotent.
func (h *AnalysisHandler) recordFindingAlert(ctx context.Context, a *storageif.GovernanceAlert) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("finding alert recording panicked", "err", r)
		}
	}()
	if h.backend == nil || a == nil {
		return
	}
	a.CreatedAt = time.Now().UTC()
	if err := h.backend.RecordGovernanceAlert(ctx, a); err != nil {
		logger.Warn("failed to record finding alert", "type", a.Type, "err", err)
	}
}

// callerDisplayName resolves a display name for alert copy ("you were assigned by X"): the user's
// display name, else their email, else the fallback (claims email, no lookup), else "a teammate".
func (h *AnalysisHandler) callerDisplayName(ctx context.Context, userID, fallback string) string {
	if h.backend != nil && userID != "" {
		if u, err := h.backend.LoadUserByID(ctx, userID); err == nil && u != nil {
			if u.DisplayName != "" {
				return u.DisplayName
			}
			if u.Email != "" {
				return u.Email
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return "a teammate"
}

// ruleDisplayName maps a rule ID to its human name for alert titles.
func (h *AnalysisHandler) ruleDisplayName(ruleID string) string {
	for _, rule := range analyzer.AllRules() {
		if rule.ID() == ruleID {
			return rule.Name()
		}
	}
	if ruleID == "" {
		return "A finding"
	}
	return ruleID
}

// notifyFindingAssignment emails the assignee. Best-effort: any error is logged and swallowed — the
// triage write already succeeded. The assigner's name arrives resolved (fanOutAssignment).
func (h *AnalysisHandler) notifyFindingAssignment(ctx context.Context, assigneeID, assignerName, flowName, ruleID string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("finding-assignment notification panicked", "err", r)
		}
	}()
	if h.email == nil || h.backend == nil {
		return
	}
	assignee, err := h.backend.LoadUserByID(ctx, assigneeID)
	if err != nil || assignee == nil || assignee.Email == "" {
		return
	}
	assigneeName := assignee.DisplayName
	if assigneeName == "" {
		assigneeName = assignee.Email
	}
	// Use the rule's human-readable name as the finding title when available.
	findingTitle := h.ruleDisplayName(ruleID)
	if err := h.email.SendFindingAssigned(ctx, assignee.Email, assigneeName, assignerName, flowName, findingTitle); err != nil {
		logger.Warn("failed to send finding-assignment notification", "assignee", assigneeID, "error", err)
	}
}

// handleBatchSetFindingStatus applies one lifecycle update to many findings of a flow in a single
// request. Authorization is checked once; all items are validated up-front so a bad item rejects
// the whole batch before any write.
// @Summary      Batch-set triage statuses for multiple findings
// @Tags         triage
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/triage/set-batch [post]
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
	if !decodeBody(w, r, &req) {
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
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}

	items := make([]*storageif.FindingStatus, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, &storageif.FindingStatus{
			FlowID:        req.FlowID,
			FindingKey:    it.FindingKey,
			RuleID:        it.RuleID,
			Status:        it.Status,
			Justification: it.Justification,
			AssigneeID:    it.AssigneeID,
			UpdatedBy:     userID,
		})
	}
	// Atomic: all items commit or none (a single RLS-scoped tx) — the old per-item loop committed
	// 1..K-1 on a mid-batch failure, desyncing the audit's "updated: N" from reality.
	if err := h.backend.BatchSetFindingStatus(r.Context(), req.FlowID, userID, items); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFindingTriage, "flow", req.FlowID,
		map[string]string{"batch": fmt.Sprintf("%d", len(req.Items)), "status": req.Items[0].Status})
	render.JSON(w, map[string]int{"updated": len(req.Items)})
}

// @Summary      Clear triage status for a finding
// @Tags         triage
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/triage/clear [post]
func (h *AnalysisHandler) handleClearFindingStatus(w http.ResponseWriter, r *http.Request) {
	if !h.triageAvailable(w) {
		return
	}
	var req struct {
		FlowID     string `json:"flowId"`
		FindingKey string `json:"findingKey"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.FindingKey == "" {
		render.Error(w, fmt.Errorf("findingKey is required"), http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}

	if err := h.backend.DeleteFindingStatus(r.Context(), req.FlowID, req.FindingKey); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Get the baseline snapshot for a flow
// @Tags         baseline
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/baseline/get [post]
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
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "viewer"); err != nil {
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

// @Summary      Set the current analysis as the baseline
// @Tags         baseline
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/baseline/set [post]
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

	// Snapshot the current findings' content-stable fingerprints as the accepted baseline
	// (survive re-imports/re-parses, unlike the legacy RuleID:BlockID key).
	seen := make(map[string]bool, len(report.Findings))
	keys := make([]string, 0, len(report.Findings))
	for _, f := range report.Findings {
		k := f.Fingerprint
		if k == "" {
			k = f.Key()
		}
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

// @Summary      Clear the baseline for a flow
// @Tags         baseline
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/baseline/clear [post]
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
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "editor"); err != nil {
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

// handleBaselineDrift reports findings introduced since the accepted baseline (the "new since
// baseline" ratchet). With no baseline set, every finding is reported as new (HasBaseline=false).
// @Summary      Compare current analysis against the baseline
// @Tags         baseline
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/baseline/drift [post]
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

// registerTriageRoutes wires the finding-triage and baseline sub-routes
// (persistent, team-shared; cloud mode only).
func (h *AnalysisHandler) registerTriageRoutes(r chi.Router) {
	r.Post("/triage/list", h.handleListFindingStatuses)
	r.Post("/triage/set", h.handleSetFindingStatus)
	r.Post("/triage/set-batch", h.handleBatchSetFindingStatus)
	r.Post("/triage/clear", h.handleClearFindingStatus)
	r.Post("/baseline/get", h.handleGetBaseline)
	r.Post("/baseline/set", h.handleSetBaseline)
	r.Post("/baseline/clear", h.handleClearBaseline)
	r.Post("/baseline/drift", h.handleBaselineDrift)
}
