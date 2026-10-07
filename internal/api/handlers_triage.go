package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/logger"
)

// Persistent, team-shared finding triage & baselines. A finding's status/justification is pinned to
// its stable key (models.Finding.Key) so it survives re-analysis; a baseline snapshots the accepted
// set of keys for ratcheting. Requires the server-side store — in desktop mode these return 503
// (registerTriageRoutes).

// validTriageStatuses is the allowed lifecycle for a finding's triage state.
var validTriageStatuses = map[string]bool{
	"open":         true,
	"acknowledged": true,
	"in_progress":  true,
	"resolved":     true,
	"suppressed":   true,
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
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, service.FlowRead); err != nil {
		render.Error(w, err, 0)
		return
	}

	statuses, err := h.triage.ListStatuses(r.Context(), req.FlowID)
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
	flow, err := h.flowSvc.GetAuthorizedHeader(r.Context(), req.FlowID, service.FlowEdit)
	if err != nil {
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
	// An actual assignment change (not a status tweak re-sending the same assignee) publishes
	// FindingAssigned: OnFindingAssigned alerts and emails the assignee after the commit.
	if err := h.triage.SetStatus(r.Context(), service.FlowRefOf(flow), st); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFindingTriage, ResourceType: "flow", ResourceID: req.FlowID, OrgID: flowOrgID(flow),
		Meta: map[string]string{"findingKey": req.FindingKey, "status": req.Status}})

	render.JSON(w, st)
}

// OnFindingAssigned is the async subscriber of events.FindingAssigned (subscribed in internal/di): the
// assignment's fan-out, after the commit and never for a rolled-back assignment.
func (h *AnalysisHandler) OnFindingAssigned(ctx context.Context, e events.FindingAssigned) {
	actorEmail := claimsEmail(ctx, e.AssignerID)
	h.goFanOut(func() {
		h.fanOutAssignment(e.FlowID, e.FindingKey, e.RuleID, e.FlowName, e.AssigneeID, e.AssignerID, actorEmail)
	})
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
	assignerName := h.triage.DisplayName(ctx, assignerID, assignerEmail)
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
	if err := h.triage.RecordAlert(ctx, a); err != nil {
		logger.Warn("failed to record finding alert", "type", a.Type, "err", err)
	}
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
	assignee := h.triage.Recipient(ctx, assigneeID)
	if assignee == nil {
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
	// The header the check reads names the flow's owner and organisation in the event the write publishes.
	flow, err := h.flowSvc.GetAuthorizedHeader(r.Context(), req.FlowID, service.FlowEdit)
	if err != nil {
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
	if err := h.triage.SetStatusBatch(r.Context(), service.FlowRefOf(flow), userID, items); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFindingTriage, ResourceType: "flow", ResourceID: req.FlowID,
		Meta: map[string]string{"batch": fmt.Sprintf("%d", len(req.Items)), "status": req.Items[0].Status}})
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

	flow, err := h.flowSvc.GetAuthorizedHeader(r.Context(), req.FlowID, service.FlowEdit)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	if err := h.triage.ClearStatus(r.Context(), service.FlowRefOf(flow), req.FindingKey); err != nil {
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
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, service.FlowRead); err != nil {
		render.Error(w, err, 0)
		return
	}

	bl, err := h.triage.Baseline(r.Context(), req.FlowID)
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
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, service.FlowEdit)
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
	if err := h.triage.SetBaseline(r.Context(), service.FlowRef{ID: req.FlowID, Name: doc.Name, OwnerID: doc.OwnerID, OrgID: doc.OrganizationID}, bl); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionBaselineSet, ResourceType: "flow", ResourceID: req.FlowID, OrgID: doc.OrganizationID,
		Meta: map[string]string{"keys": fmt.Sprintf("%d", len(keys))}})
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
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	flow, err := h.flowSvc.GetAuthorizedHeader(r.Context(), req.FlowID, service.FlowEdit)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	if err := h.triage.ClearBaseline(r.Context(), service.FlowRefOf(flow)); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionBaselineClear, ResourceType: "flow", ResourceID: req.FlowID})
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
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, service.FlowRead)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	report, err := h.analysisSvc.AnalyzeFlow(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	bl, err := h.triage.Baseline(r.Context(), req.FlowID)
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
// (persistent, team-shared: they need the server-side store).
func (h *AnalysisHandler) registerTriageRoutes(rb *route.Builder) {
	rb.Group(func(rb *route.Builder) {
		rb.Post("/triage/list", h.handleListFindingStatuses)
		rb.Post("/triage/set", h.handleSetFindingStatus)
		rb.Post("/triage/set-batch", h.handleBatchSetFindingStatus)
		rb.Post("/triage/clear", h.handleClearFindingStatus)
		rb.Post("/baseline/get", h.handleGetBaseline)
		rb.Post("/baseline/set", h.handleSetBaseline)
		rb.Post("/baseline/clear", h.handleClearBaseline)
		rb.Post("/baseline/drift", h.handleBaselineDrift)
	}, route.Needs(capability.ServerStore))
}
