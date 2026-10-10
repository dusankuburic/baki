package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
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
	triage      *service.TriageService
	comments    *service.CommentService
	shares      *service.ShareTokenService
	security    *SecurityConfig
	backend     storageif.StorageBackend
	audit       *audit.Recorder
	policies    *service.PolicyService
	governance  *service.GovernanceAlertService
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
	// exports renders the SARIF/JUnit/CSV bodies of analyze-raw, /export and the CI webhook
	// through the export registry.
	exports *service.ExportService
}

// maxConcurrentFindingEmails caps the detached finding emails in flight.
const maxConcurrentFindingEmails = 32

// maxConcurrentFindingAlerts caps the detached alert fan-outs: cheap DB writes
// that must not queue behind SMTP, so they have their own larger pool.
const maxConcurrentFindingAlerts = 64

// maxConcurrentAnalysisRecords caps the detached dashboard writes; past the cap the write is dropped
// (pad_background_dropped_total) and the next fresh analyze retries it.
const maxConcurrentAnalysisRecords = 32

func NewAnalysisHandler(analysisSvc *service.AnalysisService, flowSvc *service.FlowService, dashboard *service.DashboardService, backend storageif.StorageBackend, security *SecurityConfig, webhook *service.WebhookNotifier, email *mailer.Service, ciSecret CIWebhookSecret, policies *service.PolicyService, governance *service.GovernanceAlertService, triage *service.TriageService, comments *service.CommentService, shares *service.ShareTokenService, recorder *audit.Recorder, exports *service.ExportService) *AnalysisHandler {
	return &AnalysisHandler{
		analysisSvc: analysisSvc,
		flowSvc:     flowSvc,
		dashboard:   dashboard,
		triage:      triage,
		comments:    comments,
		shares:      shares,
		security:    security,
		backend:     backend,
		audit:       recorder,
		policies:    policies,
		governance:  governance,
		webhook:     webhook,
		email:       email,
		ciSecret:    ciSecret,
		exports:     exports,
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

	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, service.FlowRead)
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
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowAnalyze, ResourceType: "flow", ResourceID: req.FlowID, OrgID: doc.OrganizationID})
	render.JSON(w, res)
}

// @Summary      Analyze raw PAD source
// @Description  handleAnalyzeRaw analyzes raw PAD flow text WITHOUT requiring a pre-stored flow, so CI pipelines and wrappers can POST flow text and get findings back in one call — no library upload, no Go CLI install. Works in both modes (auth via PAT in cloud; open in local). Body: {files, name, format?} where format is "json" (default: the analysis report), "sarif" (SARIF 2.1.0), "junit" (JUnit XML) or "csv"; those three are sent as an attachment named pad-analysis.<ext>. At most 50 files and 2048 KB in total. Nothing is persisted.
// @Tags         analysis
// @Accept       json
// @Produce      json,application/sarif+json,application/xml,text/csv
// @Param        request body object true "{files: {name: source}, name, format}"
// @Success      200 {object} map[string]interface{} "Analysis report (json), or the SARIF, JUnit or CSV document"
// @Failure      400 {object} map[string]string "Bad Request (no files, unknown format, parse failure)"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      413 {object} map[string]string "Too many files or payload too large"
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

	if req.Format != "" && req.Format != "json" && !slices.Contains(analyzeRawFormats, req.Format) {
		render.Error(w, fmt.Errorf("unknown format %q (must be %s)", req.Format, formatChoices(append([]string{"json"}, analyzeRawFormats...))), http.StatusBadRequest)
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
	report, err := h.analysisSvc.AnalyzeRaw(ctx, doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if req.Format == "" || req.Format == "json" {
		render.JSON(w, report)
		return
	}
	body, f, err := h.exports.Render(req.Format, export.Input{Report: report, Doc: doc}, export.Options{})
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	writeDownload(w, f, body)
}

// analyzeRawFormats are the registered export formats analyze-raw renders besides its default
// JSON report. The set is the route's own: the documents (HTML, Markdown, PDF) are not offered on
// this PAT-authenticated CI endpoint.
var analyzeRawFormats = []string{"sarif", "junit", "csv"}

// exportBaseName names every report download: pad-analysis plus the format's extension.
const exportBaseName = "pad-analysis"

// writeDownload sends a rendered export as a file: the format's Content-Type, an attachment
// named after the format, and the bytes exactly as rendered.
func writeDownload(w http.ResponseWriter, f export.Format, body []byte) {
	w.Header().Set("Content-Type", f.MIME)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", exportBaseName+f.Ext))
	_, _ = w.Write(body)
}

// formatChoices lists format ids for an error message: "a, b, or c".
func formatChoices(ids []string) string {
	if len(ids) < 3 {
		return strings.Join(ids, " or ")
	}
	return strings.Join(ids[:len(ids)-1], ", ") + ", or " + ids[len(ids)-1]
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
// @Description  SARIF 2.1.0 report of the flow's analysis (GitHub Code Scanning and other SARIF consumers), sent as the attachment pad-analysis.sarif. Analyzes on demand (a fresh analysis records history, as analyze does). An empty flowId means the current document in local mode.
// @Tags         analysis
// @Accept       json
// @Produce      application/sarif+json
// @Param        request body object false "{flowId}"
// @Success      200 {string} string "SARIF document"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Flow not found"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/export/sarif [post]
func (h *AnalysisHandler) handleExportSARIF(w http.ResponseWriter, r *http.Request) {
	h.serveFlowExport(w, r, "sarif")
}

// @Summary      Export analysis results as JUnit XML
// @Description  JUnit XML report of the flow's analysis (CI test-report viewers), sent as the attachment pad-analysis.xml. Analyzes on demand, like the SARIF export. An empty flowId means the current document in local mode.
// @Tags         export
// @Accept       json
// @Produce      application/xml
// @Param        request body object false "{flowId}"
// @Success      200 {string} string "JUnit XML document"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Flow not found"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/analysis/export/junit [post]
func (h *AnalysisHandler) handleExportJUnit(w http.ResponseWriter, r *http.Request) {
	h.serveFlowExport(w, r, "junit")
}

// serveFlowExport analyzes the requested flow and sends its report, rendered as the export
// format id, as a download.
func (h *AnalysisHandler) serveFlowExport(w http.ResponseWriter, r *http.Request, id string) {
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

	body, f, err := h.exports.Render(id, export.Input{Report: report, Doc: doc}, export.Options{})
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowExport, ResourceType: "flow", ResourceID: req.FlowID, OrgID: doc.OrganizationID, Meta: map[string]string{"format": f.ID}})
	writeDownload(w, f, body)
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
// unauthenticated /shared shell, so callers outside the handler (the router's
// static serving) do not reach into its fields; nil means the token is unknown
// or the handler is not fully wired. The header, not the resolved document:
// only the flow's name is injected on this route, and resolving the full
// document per hit of a leaked link was the most expensive thing a stranger
// could make the server do.
func (h *AnalysisHandler) LookupSharedFlowHeader(ctx context.Context, token string) *storageif.FlowDocument {
	if h == nil || h.backend == nil || h.flowSvc == nil || h.shares == nil || token == "" {
		return nil
	}
	return h.shares.SharedFlowHeader(ctx, token)
}

// Mount registers the /api/analysis group (analyze/batch/exports here;
// findings-introspection, rules, suppressions, triage, policy and comment
// sub-groups live with their handlers), the governance alerts inbox, the
// share-link report and the inbound CI webhook.
func (h *AnalysisHandler) Mount(rb *route.Builder) {
	// Unauthenticated (?token=...), and each first hit of a leaked link pays a
	// full analyzer walk: the analysis bucket. Share links live in the
	// server-side store.
	rb.Get("/api/shared", h.handleViewShared, route.Public(), route.RateLimit(route.RateAnalysis), route.Needs(capability.ServerStore))
	rb.Post("/api/integrations/ci", h.handleCIWebhook, route.SelfAuthenticated()) // HMAC (X-Baki-Signature)

	// Every POST in the tree rides the analysis bucket; the reads stay on general.
	rb.Route("/api/analysis", func(rb *route.Builder) {
		// Seconds of parse/analyze/export CPU (analyze-raw persists nothing,
		// batch is desktop-only): outside the request transaction.
		rb.Post("/analyze", h.handleAnalyzeFlow, route.NoTx())
		rb.Post("/analyze-raw", h.handleAnalyzeRaw, route.NoTx())
		rb.Post("/batch", h.handleBatchAnalyze, route.NoTx())
		rb.Post("/export/sarif", h.handleExportSARIF, route.NoTx())
		rb.Post("/export/junit", h.handleExportJUnit, route.NoTx())
		rb.Get("/dashboard", h.handleGetDashboard, route.RateLimit(route.RateGeneral))
		h.registerFindingRoutes(rb)
		h.registerAnalysisRuleRoutes(rb)
		h.registerSuppressionRoutes(rb)
		h.registerTriageRoutes(rb)
		h.registerPolicyRoutes(rb)
		h.registerCommentRoutes(rb)
	}, route.RateLimit(route.RateAnalysis))
	h.registerGovernanceRoutes(rb)
}
