package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/lifecycle"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
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

// LookupSharedFlowHeader resolves a share token to its flow's header for the
// unauthenticated /shared shell. Token hashing and the storage lookups live
// here so callers outside the handler (the router's static serving) do not
// reach into its fields; nil means the token is unknown or the handler is not
// fully wired. The header, not the resolved document: only the flow's name is
// injected on this route, and resolving the full document per hit of a leaked
// link was the most expensive thing a stranger could make the server do.
func (h *AnalysisHandler) LookupSharedFlowHeader(ctx context.Context, token string) *storageif.FlowDocument {
	if h == nil || h.backend == nil || h.flowSvc == nil || token == "" {
		return nil
	}
	hash := sha256.Sum256([]byte(token))
	st, err := h.backend.GetShareTokenByHash(ctx, hex.EncodeToString(hash[:]))
	if err != nil || st == nil {
		return nil
	}
	doc, err := h.backend.LoadFlowHeader(ctx, st.FlowID)
	if err != nil || doc == nil {
		return nil
	}
	return doc
}

// registerRoutes wires the /api/analysis group: analyze/batch/exports here;
// findings-introspection, rules, suppressions, triage, policy and comment
// sub-groups live with their handlers.
func (h *AnalysisHandler) registerRoutes(r chi.Router) {
	r.Route("/api/analysis", func(r chi.Router) {
		r.Post("/analyze", h.handleAnalyzeFlow)
		r.Post("/analyze-raw", h.handleAnalyzeRaw)
		r.Post("/batch", h.handleBatchAnalyze)
		r.Post("/export/sarif", h.handleExportSARIF)
		r.Post("/export/junit", h.handleExportJUnit)
		r.Get("/dashboard", h.handleGetDashboard)
		h.registerFindingRoutes(r)
		h.registerAnalysisRuleRoutes(r)
		h.registerSuppressionRoutes(r)
		h.registerTriageRoutes(r)
		h.registerPolicyRoutes(r)
		h.registerCommentRoutes(r)
	})
}
