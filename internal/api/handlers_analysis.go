package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func (h *AnalysisHandler) handleGetRules(w http.ResponseWriter, r *http.Request) {
	rules := h.analysisSvc.GetRules()
	render.JSON(w, rules)
}

func (h *AnalysisHandler) handleGetRulesSummary(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, h.analysisSvc.GetRulesSummary())
}

func (h *AnalysisHandler) handleSetRuleEnabled(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleMember) {
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

func (h *AnalysisHandler) handleUpdateRuleConfig(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleMember) {
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

func (h *AnalysisHandler) handleExportHTML(w http.ResponseWriter, r *http.Request) {
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

	html := h.analysisSvc.GenerateHTMLReport(report)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline; filename=\"analysis-report.html\"")
	_, _ = w.Write([]byte(html))
}

// handleExportSARIF emits a SARIF 2.1.0 report for the flow, suitable for
// GitHub Code Scanning or any SARIF-consuming tool. Mirrors handleExportHTML
// but uses the SARIF serializer instead of the HTML generator.
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

func (h *AnalysisHandler) handleGetDependencies(w http.ResponseWriter, r *http.Request) {
	result := h.analysisSvc.GetDependencyAnalysis()
	render.JSON(w, result)
}

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

func decodeOptional(r io.Reader, v any) error {
	if r == nil {
		return nil
	}
	if err := json.NewDecoder(r).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
