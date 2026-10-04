package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/lifecycle"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/export"
	"pad-core/models"
	"pad-core/parser"
)

type AnalysisHandler struct {
	analysisSvc *service.AnalysisService
	flowSvc     *service.FlowService
	dashboard   *service.DashboardService
	security    *SecurityConfig
	backend     storageif.StorageBackend
	// email sends finding-assignment notifications; nil-safe (the log-only
	// mailer is always non-nil even without SMTP).
	email *mailer.Service
	// webhook is best-effort / async via PAD_WEBHOOK_URL; no-op when unset.
	// Injected for testability (was a package-global var).
	webhook *service.WebhookNotifier
	// ciSecret is the HMAC key for the inbound CI webhook (PAD_CI_WEBHOOK_SECRET).
	// Empty disables the endpoint (503). Auth is via X-Baki-Signature, not JWT.
	ciSecret CIWebhookSecret
	// emails runs the detached finding-comment/assignment emails, bounded and
	// drained on shutdown (a bare `go` per request would pile up unbounded).
	emails *lifecycle.Bounded
	// alerts runs the detached in-app alert fan-outs: DB-only, kept apart from
	// the SMTP pool so a stalled relay cannot make the badge drop.
	alerts *lifecycle.Bounded
	// records runs the detached dashboard writes of a fresh analysis: best-effort on the
	// pool (RLS-exempt, no request transaction) so they can finish after the response.
	records *lifecycle.Bounded
}

// maxConcurrentFindingEmails caps the detached finding emails in flight.
const maxConcurrentFindingEmails = 32

// maxConcurrentFindingAlerts caps the detached alert fan-outs: cheap DB writes
// that must not queue behind SMTP, so they have their own larger pool.
const maxConcurrentFindingAlerts = 64

// maxConcurrentAnalysisRecords caps the detached dashboard writes; past the cap the write is dropped
// (pad_background_dropped_total) and the next fresh analyze retries it.
const maxConcurrentAnalysisRecords = 32

func NewAnalysisHandler(analysisSvc *service.AnalysisService, flowSvc *service.FlowService, dashboard *service.DashboardService, backend storageif.StorageBackend, security *SecurityConfig, webhook *service.WebhookNotifier, email *mailer.Service, ciSecret CIWebhookSecret) *AnalysisHandler {
	return &AnalysisHandler{
		analysisSvc: analysisSvc,
		flowSvc:     flowSvc,
		dashboard:   dashboard,
		security:    security,
		backend:     backend,
		webhook:     webhook,
		email:       email,
		ciSecret:    ciSecret,
		emails:      lifecycle.NewBounded("finding_email", maxConcurrentFindingEmails),
		alerts:      lifecycle.NewBounded("finding_alert", maxConcurrentFindingAlerts),
		records:     lifecycle.NewBounded("analysis_record", maxConcurrentAnalysisRecords),
	}
}

// DrainBackground stops starting new best-effort work (finding emails, alert
// fan-outs, dashboard records, webhooks) and waits for the ones in flight.
func (h *AnalysisHandler) DrainBackground(ctx context.Context) error {
	// Alerts first: an alert job enqueues its email once the actor's name is
	// resolved, so draining emails before alerts would drop those sends.
	err := h.alerts.Drain(ctx)
	if eerr := h.emails.Drain(ctx); err == nil {
		err = eerr
	}
	if rerr := h.records.Drain(ctx); err == nil {
		err = rerr
	}
	if h.webhook != nil {
		if werr := h.webhook.Drain(ctx); err == nil {
			err = werr
		}
	}
	return err
}

// recordAnalysis persists the dashboard summary off the request path: bounded, detached, RLS-exempt.
// A dropped task gives the report's record claim back so the next analyze retries the write.
func (h *AnalysisHandler) recordAnalysis(ctx context.Context, doc *models.FlowDocument, report *models.AnalysisReport) {
	if h.dashboard == nil {
		return
	}
	detached := context.WithoutCancel(ctx)
	started := h.records.Go(func() {
		ctx, cancel := context.WithTimeout(detached, bestEffortTimeout)
		defer cancel()
		h.dashboard.RecordAnalysis(ctx, doc, report)
	})
	if !started {
		h.analysisSvc.ReleaseRecord(doc, report)
	}
}

// @Summary      Analyze current flow
// @Description  Runs all enabled analysis rules on the current flow document.
// @Tags         analysis
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/analysis/analyze [post]
func (h *AnalysisHandler) handleAnalyzeFlow(w http.ResponseWriter, r *http.Request) {
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

	res, fresh, err := h.analysisSvc.AnalyzeFlowFresh(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Side effects belong to a REPORT, not every fetch of it: a cache hit already recorded and
	// notified. fresh is the dashboard-record claim, not "new run".
	if fresh {
		// Persist the dashboard summary (best-effort, detached).
		h.recordAnalysis(r.Context(), doc, res)
		// Webhook notification (best-effort, async via PAD_WEBHOOK_URL).
		h.webhook.NotifyAnalysis(doc.Name, res)
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowAnalyze, "flow", req.FlowID, nil)
	render.JSON(w, res)
}

// @Summary      Analyze raw PAD source
// @Description  handleAnalyzeRaw analyzes raw PAD flow text WITHOUT requiring a pre-stored flow, so CI pipelines and wrappers can POST flow text and get findings JSON (or SARIF) back in one call — no library upload, no Go CLI install. Works in both modes (auth via PAT in cloud; open in local). Body: {files, name, format?} where format is "json" (default) or "sarif". Nothing is persisted.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/analyze-raw [post]
func (h *AnalysisHandler) handleAnalyzeRaw(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Files  map[string]string `json:"files"`
		Name   string            `json:"name"`
		Format string            `json:"format,omitempty"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Files) == 0 {
		render.Error(w, fmt.Errorf("no files provided"), http.StatusBadRequest)
		return
	}

	switch req.Format {
	case "", "json", "sarif", "junit", "csv":
	default:
		render.Error(w, fmt.Errorf("unknown format %q (must be json, sarif, junit, or csv)", req.Format), http.StatusBadRequest)
		return
	}

	// Bound the CPU surface of this barely-authenticated endpoint: a 10 MB body of pathological PAD
	// source can burn seconds in parse+analyze; the per-request deadline bounds the HTTP layer only.
	const (
		maxRawFiles   = 50
		maxRawTotalKB = 2048
		rawAnalyzeTTL = 30 * time.Second
	)
	if len(req.Files) > maxRawFiles {
		render.Error(w, fmt.Errorf("too many files (%d > %d)", len(req.Files), maxRawFiles), http.StatusRequestEntityTooLarge)
		return
	}
	var totalKB int
	for _, src := range req.Files {
		totalKB += (len(src) + 1023) / 1024
	}
	if totalKB > maxRawTotalKB {
		render.Error(w, fmt.Errorf("payload too large (%d KB > %d KB)", totalKB, maxRawTotalKB), http.StatusRequestEntityTooLarge)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), rawAnalyzeTTL)
	defer cancel()

	// Parse purely (no docProvider side effect — this is stateless).
	doc, err := parser.ParseFiles(req.Files, req.Name)
	if err != nil {
		render.Error(w, fmt.Errorf("parse failed: %w", err), http.StatusBadRequest)
		return
	}
	report, err := h.analysisSvc.AnalyzeFlow(ctx, doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if req.Format == "sarif" {
		out, err := export.ReportToSARIF(report, doc)
		if err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/sarif+json")
		fmt.Fprintf(w, "%s\n", out)
		return
	}
	if req.Format == "junit" {
		out, err := export.ReportToJUnit(report, doc)
		if err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		fmt.Fprintf(w, "%s\n", out)
		return
	}
	if req.Format == "csv" {
		out, err := export.ReportToCSV(report, doc)
		if err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="pad-analysis.csv"`)
		fmt.Fprintf(w, "%s", out)
		return
	}
	render.JSON(w, report)
}

// @Summary      Get variable lineage
// @Description  Returns the history and dependencies of a specific variable within the flow.
// @Tags         analysis
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/analysis/lineage [post]
func (h *AnalysisHandler) handleGetVariableLineage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Variable string `json:"varName"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Variable == "" {
		render.Error(w, fmt.Errorf("varName is required"), http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "viewer")
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	history, err := h.analysisSvc.GetVariableLineage(doc, req.Variable)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, history)
}

// @Summary      Get execution graph
// @Description  Returns a graph representation of the flow's execution path.
// @Tags         analysis
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/analysis/graph [post]
func (h *AnalysisHandler) handleGetExecutionGraph(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flowId")
	if flowID == "" && r.Body != nil {
		var req struct {
			FlowID string `json:"flowId"`
		}
		if err := decodeOptional(r.Body, &req); err != nil {
			render.Error(w, err, http.StatusBadRequest)
			return
		}
		flowID = req.FlowID
	}

	userID := h.security.CallerID(r)
	doc, err := h.flowSvc.GetAuthorized(r.Context(), flowID, userID, "viewer")
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	graph, err := h.analysisSvc.GetExecutionGraph(doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, graph)
}

// @Summary      List analysis rules
// @Description  Returns all available analysis rules and their current configuration.
// @Tags         analysis
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
// @Param        orgId query string false "Scope the catalog to an organisation the caller belongs to (org custom rules + overrides)"
// @Router       /api/analysis/rules [get]
func (h *AnalysisHandler) handleGetRules(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.catalogOrg(w, r)
	if !ok {
		return
	}
	render.JSON(w, h.analysisSvc.GetRulesFor(r.Context(), orgID))
}

// catalogOrg reads the optional ?orgId= for the rule catalog endpoints and
// checks the caller is a member — org custom rules are tenant data.
func (h *AnalysisHandler) catalogOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	orgID := r.URL.Query().Get("orgId")
	if orgID == "" {
		return "", true
	}
	if !requireOrgMember(w, r, h.security, orgID) {
		return "", false
	}
	return orgID, true
}

// handleValidateCustomRules validates a custom-rules payload without loading
// it — the same checks `bakicli rules test` applies.
// @Summary      Validate custom rules
// @Description  Validates custom-rule JSON without installing it: per-entry validity, errors, and skip warnings.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Results"
// @Router       /api/analysis/custom-rules/validate [post]
func (h *AnalysisHandler) handleValidateCustomRules(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var raw json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}
	// Accept a raw array, a single object, or the {"rules": …} envelope.
	var payload struct {
		Rules json.RawMessage `json:"rules"`
	}
	inner := raw
	if err := json.Unmarshal(raw, &payload); err == nil && len(payload.Rules) > 0 {
		inner = payload.Rules
	}
	var configs []analyzer.CustomRuleConfig
	if err := json.Unmarshal(inner, &configs); err != nil {
		var one analyzer.CustomRuleConfig
		if err2 := json.Unmarshal(inner, &one); err2 != nil || one.ID == "" {
			render.Error(w, fmt.Errorf("not a custom-rule JSON array or object"), http.StatusBadRequest)
			return
		}
		configs = []analyzer.CustomRuleConfig{one}
	}
	type entryResult struct {
		Index  int    `json:"index"`
		ID     string `json:"id"`
		Valid  bool   `json:"valid"`
		Error  string `json:"error,omitempty"`
		Loaded bool   `json:"loaded"`
	}
	out := make([]entryResult, len(configs))
	valid := 0
	for i, cfg := range configs {
		out[i] = entryResult{Index: i, ID: cfg.ID}
		if _, err := analyzer.NewCustomRule(cfg); err != nil {
			out[i].Error = err.Error()
			continue
		}
		out[i].Valid = true
		valid++
	}
	render.JSON(w, map[string]any{"entries": out, "valid": valid, "invalid": len(configs) - valid})
}

// @Summary      Enable/disable analysis rule
// @Description  Enables or disables a specific analysis rule by ID.
// @Tags         analysis
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/analysis/rule/enabled [post]
func (h *AnalysisHandler) handleSetRuleEnabled(w http.ResponseWriter, r *http.Request) {
	// System admin, not member: this writes the DEPLOYMENT-WIDE settings singleton — any org member
	// could otherwise change analysis for every tenant. Per-org configuration lives at /api/orgs/{id}/rules.
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	// The frontend sends "ruleId"; "id" kept for compat — decoding only "id" wrote to rule "" silently.
	var req struct {
		ID      string `json:"id"`
		RuleID  string `json:"ruleId"`
		Enabled bool   `json:"enabled"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.RuleID != "" {
		req.ID = req.RuleID
	}
	if req.ID == "" {
		render.Error(w, fmt.Errorf("ruleId is required"), http.StatusBadRequest)
		return
	}
	if err := h.analysisSvc.SetRuleEnabled(r.Context(), req.ID, req.Enabled); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleTestCustomRule runs a CANDIDATE rule against one of the caller's flows and reports what it
// would match — validate answers "does this compile", this "does it do anything". Member-level, stores nothing.
// @Summary      Test a candidate custom rule against a flow
// @Description  Runs one unsaved rule against a flow and returns the findings it would produce.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Matches"
// @Failure      400 {object} map[string]string "Bad Request"
// @Router       /api/analysis/custom-rules/test [post]
func (h *AnalysisHandler) handleTestCustomRule(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleMember) {
		return
	}
	var req struct {
		Rule   analyzer.CustomRuleConfig `json:"rule"`
		FlowID string                    `json:"flowId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Rule.ID == "" {
		render.Error(w, fmt.Errorf("rule.id is required"), http.StatusBadRequest)
		return
	}
	if req.FlowID == "" {
		render.Error(w, fmt.Errorf("flowId is required — pick a flow to test the rule against"), http.StatusBadRequest)
		return
	}

	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, h.security.CallerID(r), "viewer")
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	findings, err := h.analysisSvc.TestRule(r.Context(), doc, req.Rule)
	if err != nil {
		// A rule that does not compile is the AUTHOR's error, not a server fault — same 400 as the save endpoint.
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	render.JSON(w, map[string]any{
		"matches":  len(findings),
		"findings": findings,
		"flowName": doc.Name,
	})
}

// @Summary      Update analysis rule config
// @Description  Updates the configuration parameters for a specific analysis rule.
// @Tags         analysis
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/analysis/rule/config [post]
func (h *AnalysisHandler) handleUpdateRuleConfig(w http.ResponseWriter, r *http.Request) {
	// System admin — same reasoning as handleSetRuleEnabled: deployment-wide settings singleton.
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	// Same id/ruleId compatibility shim as handleSetRuleEnabled.
	var req struct {
		ID     string            `json:"id"`
		RuleID string            `json:"ruleId"`
		Config models.RuleConfig `json:"config"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.RuleID != "" {
		req.ID = req.RuleID
	}
	if req.ID == "" {
		render.Error(w, fmt.Errorf("ruleId is required"), http.StatusBadRequest)
		return
	}
	if err := h.analysisSvc.UpdateRuleConfig(r.Context(), req.ID, req.Config); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Dataflow analysis
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/dataflow [post]
func (h *AnalysisHandler) handleGetDataFlow(w http.ResponseWriter, r *http.Request) {
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

	if doc == nil {
		render.Error(w, fmt.Errorf("no flow loaded"), http.StatusInternalServerError)
		return
	}
	// Bounded and cancellable: the edges are O(readers x writers) per flow-global variable — on a
	// 10 MB flow that was 26M edges, ~1 GB of JSON and 18 s of CPU that kept running after the client left.
	df, err := analyzer.AnalyzeDataFlowCtx(r.Context(), doc, analyzer.DefaultDataFlowLimits)
	if err != nil {
		render.Error(w, err, http.StatusServiceUnavailable)
		return
	}
	render.JSON(w, df)
}

// @Summary      Batch-analyze a folder
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Param        request body object true "Folder path"
// @Success      200 {object} map[string]interface{} "Batch results"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/batch [post]
func (h *AnalysisHandler) handleBatchAnalyze(w http.ResponseWriter, r *http.Request) {
	// Desktop-only: reads .txt files from a server-side folder path; in
	// cloud mode it would be an authenticated arbitrary-directory read (LFI).
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("batch folder analysis is not available in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		FolderPath string `json:"folderPath"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.FolderPath == "" {
		render.Error(w, fmt.Errorf("folderPath is required"), http.StatusBadRequest)
		return
	}

	docs, loadErrors, err := h.flowSvc.LoadAllFromFolder(r.Context(), req.FolderPath)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	// No aggregate-vs-per-file invalidation needed: the analytics cache evicts overlapping path identities on Put.

	// A folder where every file failed still yields all-error rows.
	var batch *models.BatchAnalysis
	if len(docs) > 0 {
		batch, err = h.analysisSvc.AnalyzeBatch(docs)
		if err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
	} else {
		batch = &models.BatchAnalysis{Results: []models.BatchResult{}}
	}
	for name, msg := range loadErrors {
		batch.Results = append(batch.Results, models.BatchResult{FlowName: name, Error: msg})
	}
	batch.TotalFlows = len(batch.Results)
	render.JSON(w, batch)
}

// @Summary      Diff current vs. stored report
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/diff [post]
func (h *AnalysisHandler) handleDiff(w http.ResponseWriter, r *http.Request) {
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

	newReport, err := h.analysisSvc.AnalyzeFlow(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	// Diff against the previous distinct run. With no prior run the diff is
	// "everything added" by construction; HasPrevious lets the UI say so.
	oldReport, hasPrevious := h.analysisSvc.PreviousReport(doc)
	if oldReport == nil {
		oldReport = &models.AnalysisReport{Findings: []models.Finding{}}
	}
	diff := h.analysisSvc.DiffReports(oldReport, newReport)
	diff.HasPrevious = hasPrevious
	render.JSON(w, diff)
}

// @Summary      Analysis snapshot history
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/history [post]
func (h *AnalysisHandler) handleGetHistory(w http.ResponseWriter, r *http.Request) {
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

	snapshots := h.analysisSvc.History(doc)
	if snapshots == nil {
		snapshots = []analyzer.AnalysisSnapshot{}
	}
	render.JSON(w, snapshots)
}

// @Summary      Export SARIF report
// @Description  SARIF 2.1.0 report for GitHub Code Scanning.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Param        request body object true "Flow reference"
// @Success      200 {object} map[string]interface{} "SARIF document"
// @Router       /api/analysis/export/sarif [post]
func (h *AnalysisHandler) handleExportSARIF(w http.ResponseWriter, r *http.Request) {
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

	out, err := export.ReportToSARIF(report, doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowExport, "flow", req.FlowID, map[string]string{"format": "sarif"})
	w.Header().Set("Content-Type", "application/sarif+json")
	w.Header().Set("Content-Disposition", `attachment; filename="pad-analysis.sarif"`)
	fmt.Fprintf(w, "%s\n", out)
}

// @Summary      Export analysis results as JUnit XML
// @Tags         export
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/export/junit [post]
func (h *AnalysisHandler) handleExportJUnit(w http.ResponseWriter, r *http.Request) {
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

	out, err := export.ReportToJUnit(report, doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowExport, "flow", req.FlowID, map[string]string{"format": "junit"})
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="pad-analysis.xml"`)
	fmt.Fprintf(w, "%s\n", out)
}

// @Summary      Subflow dependency graph
// @Tags         analysis
// @Produce      json
// @Success      200 {object} map[string]interface{} "Dependencies"
// @Router       /api/analysis/dependencies [get]
func (h *AnalysisHandler) handleGetDependencies(w http.ResponseWriter, r *http.Request) {
	result := h.analysisSvc.GetDependencyAnalysis()
	render.JSON(w, result)
}

// @Summary      Portfolio dashboard stats
// @Tags         analysis
// @Produce      json
// @Success      200 {object} map[string]interface{} "Dashboard"
// @Router       /api/analysis/dashboard [get]
func (h *AnalysisHandler) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
	// Desktop-only: ComputeDashboard aggregates the process-global analyzer
	// cache — a cross-tenant leak in cloud; cloud uses /api/dashboard/home.
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("session analytics are not available in cloud mode; use the home dashboard"), http.StatusForbidden)
		return
	}
	result := h.analysisSvc.ComputeDashboard()
	render.JSON(w, result)
}

// @Summary      Deduplicate findings
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/deduplicate [post]
func (h *AnalysisHandler) handleDeduplicate(w http.ResponseWriter, r *http.Request) {
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

	deduped, groups := h.analysisSvc.DeduplicateFindings(report)
	render.JSON(w, map[string]interface{}{
		"deduplicated":  deduped,
		"groups":        groups,
		"originalCount": len(report.Findings),
		"dedupedCount":  len(deduped),
	})
}

// @Summary      Related findings
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/related [post]
func (h *AnalysisHandler) handleRelatedFindings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID  string `json:"flowId"`
		BlockID string `json:"blockId"`
	}
	if !decodeBody(w, r, &req) {
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

	related := h.analysisSvc.FindRelatedFindings(report, req.BlockID)
	render.JSON(w, related)
}

// @Summary      Compare two analyses
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/compare [post]
func (h *AnalysisHandler) handleCompareFlows(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowAID string `json:"flowAId"`
		FlowBID string `json:"flowBId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	userID := h.security.CallerID(r)
	docA, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowAID, userID, "viewer")
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	docB, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowBID, userID, "viewer")
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	result := h.analysisSvc.CompareFlows(docA, docB)
	render.JSON(w, result)
}
