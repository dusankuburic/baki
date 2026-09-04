package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
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
	// email sends finding-assignment notifications to the assignee. Nil-safe
	// (the log-only mailer is always non-nil even without SMTP).
	email *mailer.Service
	// webhook is best-effort / async / env-configured via PAD_WEBHOOK_URL;
	// no-op when unset. Injected for testability (was a package-global var).
	webhook *service.WebhookNotifier
	// ciSecret is the HMAC key for the inbound CI webhook (PAD_CI_WEBHOOK_SECRET).
	// Empty disables the endpoint (503). Auth is via X-Baki-Signature, not JWT.
	ciSecret CIWebhookSecret
}

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

	res, err := h.analysisSvc.AnalyzeFlow(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Persist the latest analysis summary so the welcome dashboard stays
	// populated across sessions/replicas (best-effort; no-op in local mode).
	if h.dashboard != nil {
		h.dashboard.RecordAnalysis(r.Context(), doc, res)
	}
	// Webhook notification (best-effort, async, env-configured via
	// PAD_WEBHOOK_URL). No-op if unset.
	h.webhook.NotifyAnalysis(doc.Name, res)
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowAnalyze, "flow", req.FlowID, nil)
	render.JSON(w, res)
}

// handleAnalyzeRaw analyzes raw PAD flow text WITHOUT requiring a pre-stored
// flow, so CI pipelines and wrappers can POST flow text and get findings JSON
// (or SARIF) back in one call — no library upload, no Go CLI install. Works in
// both modes (auth via PAT in cloud; open in local). Body: {files, name,
// format?} where format is "json" (default) or "sarif". Nothing is persisted.
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

	// Validate format — unknown values are rejected (consistent with the CLI).
	switch req.Format {
	case "", "json", "sarif", "junit", "csv":
	default:
		render.Error(w, fmt.Errorf("unknown format %q (must be json, sarif, junit, or csv)", req.Format), http.StatusBadRequest)
		return
	}

	// Bound the CPU surface of this unauthenticated-ish endpoint: a 10 MB body
	// of pathological PAD source (deeply nested blocks) can burn seconds in
	// parse+analyze. A per-request deadline bounds the HTTP layer (the analysis
	// engine itself runs to completion today; full enforcement needs ctx checks
	// inside RunAnalysis). Rejecting oversized payloads up front is the primary
	// guard against a single request degrading the shared backend.
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
// @Router       /api/analysis/rules [get]
func (h *AnalysisHandler) handleGetRules(w http.ResponseWriter, r *http.Request) {
	rules := h.analysisSvc.GetRules()
	render.JSON(w, rules)
}

// handleValidateCustomRules validates a custom-rules payload (array or single
// object) WITHOUT loading it: per-entry validity + error, and warnings for
// entries that would be skipped — the backend for the settings editor's
// authoring feedback (the same checks `bakicli rules test` applies).
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
	// Accept three shapes: a raw array, a single object, or the {"rules": …}
	// envelope the API client conventionally uses.
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

// @Summary      Rule catalog summary
// @Description  All rules with severity, category, and auto-fix availability.
// @Tags         analysis
// @Produce      json
// @Success      200 {object} map[string]interface{} "Rules"
// @Router       /api/analysis/rules/summary [get]
func (h *AnalysisHandler) handleGetRulesSummary(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, h.analysisSvc.GetRulesSummary())
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
	// System admin, not member. This endpoint writes the DEPLOYMENT-WIDE
	// settings singleton: before R4 any member of any org could disable a rule
	// here and silently change analysis for every tenant in the deployment,
	// with nothing surfacing that it had happened. Per-org configuration lives
	// at /api/orgs/{id}/rules and is gated on org admin.
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	// The frontend sends "ruleId"; "id" is kept for compatibility. Decoding
	// only "id" made every toggle silently write to rule "" (never matching).
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
	if err := h.analysisSvc.SetRuleEnabled(req.ID, req.Enabled); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleTestCustomRule runs a CANDIDATE rule against one of the caller's flows
// and reports what it would match.
//
// The validate endpoint answers "does this compile"; this answers "does it do
// anything". An author who cannot tell the two apart can save a regex that
// never matches and believe a policy is being enforced — the same class of
// silent no-op R1-5's suppression inventory exists to surface.
//
// Member-level and flow-authorized: it analyzes a flow the caller can already
// read, with a rule they supplied, and stores nothing.
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
		// A rule that does not compile is the AUTHOR's error, not a server
		// fault — same 400 the save endpoint gives for the same input.
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
	// System admin — same reasoning as handleSetRuleEnabled: this writes the
	// deployment-wide settings singleton, not the caller's own org.
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
	if err := h.analysisSvc.UpdateRuleConfig(req.ID, req.Config); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Flow metrics
// @Description  Complexity/cognitive metrics and health score for the flow.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/metrics [post]
func (h *AnalysisHandler) handleGetMetrics(w http.ResponseWriter, r *http.Request) {
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

	metrics, err := h.analysisSvc.GetFlowMetrics(doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, metrics)
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

	df, err := h.analysisSvc.GetDataFlow(doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
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
	// Desktop-only: this reads .txt files from a server-side folder path. In cloud
	// mode it would be an authenticated arbitrary-directory read (LFI), so gate it
	// like the other filesystem endpoints in handlers_flow.go.
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

	// No aggregate-vs-per-file invalidation needed here: the analytics cache
	// evicts overlapping path identities on Put (a per-file entry replaces any
	// folder aggregate covering it, and vice versa) — see PutWithPath.

	// A folder where every file failed still yields a useful batch result
	// (all error rows) rather than an HTTP error.
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

// handleExportSARIF emits a SARIF 2.1.0 report for the flow, suitable for
// GitHub Code Scanning or any SARIF-consuming tool.
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
	w.Header().Set("Content-Type", "application/sarif+json")
	w.Header().Set("Content-Disposition", `attachment; filename="pad-analysis.sarif"`)
	fmt.Fprintf(w, "%s\n", out)
}

// handleExportJUnit emits a JUnit XML report for the flow, suitable for CI
// pipelines (Jenkins, GitLab CI). Mirrors handleExportSARIF.
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
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="pad-analysis.xml"`)
	fmt.Fprintf(w, "%s\n", out)
}

// handleExportCSV emits a CSV report for the flow, suitable for spreadsheet
// triage. Mirrors handleExportSARIF.
// @Summary      Export findings as CSV
// @Tags         export
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/export/csv [post]
func (h *AnalysisHandler) handleExportCSV(w http.ResponseWriter, r *http.Request) {
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

	out, err := export.ReportToCSV(report, doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="pad-analysis.csv"`)
	fmt.Fprintf(w, "%s", out)
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
	// Desktop-only: ComputeDashboard aggregates the process-global analyzer cache,
	// which in cloud mode holds every tenant's analyzed flows — i.e. a cross-tenant
	// aggregate leak. Cloud clients use the owner-scoped /api/dashboard/home instead.
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("session analytics are not available in cloud mode; use the home dashboard"), http.StatusForbidden)
		return
	}
	result := h.analysisSvc.ComputeDashboard()
	render.JSON(w, result)
}

// @Summary      Subflow content hashes
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/subflow-hashes [post]
func (h *AnalysisHandler) handleGetSubflowHashes(w http.ResponseWriter, r *http.Request) {
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

	hashes := h.analysisSvc.ComputeSubflowHashes(doc)
	render.JSON(w, hashes)
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
