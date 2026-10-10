package api

import (
	"encoding/base64"
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/service"
)

type ExportHandler struct {
	exportSvc   *service.ExportService
	flowSvc     *service.FlowService
	analysisSvc *service.AnalysisService
	security    *SecurityConfig
	audit       *audit.Recorder
}

func NewExportHandler(exportSvc *service.ExportService, flowSvc *service.FlowService, analysisSvc *service.AnalysisService, security *SecurityConfig, recorder *audit.Recorder) *ExportHandler {
	return &ExportHandler{exportSvc: exportSvc, flowSvc: flowSvc, analysisSvc: analysisSvc, security: security, audit: recorder}
}

// @Summary      Compare current flow with another
// @Description  Returns a diff between the currently loaded flow and a flow at the specified path. Only available in local mode.
// @Tags         export
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/export/compare [post]
func (h *ExportHandler) handleCompareCurrentWith(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return
	}
	var req struct {
		Path   string `json:"path"`
		FlowID string `json:"flowId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, service.FlowRead)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	diff, err := h.exportSvc.CompareCurrentWith(doc, req.Path)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, diff)
}

// @Summary      Export flow as Markdown
// @Description  Renders the flow's current analysis report as Markdown and returns it base64-encoded under `data`. Needs a report (409 until the flow was analyzed). On the desktop a `path` also writes the file there; with accounts the path is ignored.
// @Tags         export
// @Param        request body object true "{flowId, path}"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "{status: ok, data: base64 Markdown}"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Flow not found"
// @Failure      409 {object} map[string]string "No analysis report"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/export/markdown [post]
func (h *ExportHandler) handleExportMarkdown(w http.ResponseWriter, r *http.Request) {
	h.serveDocExport(w, r, "markdown")
}

// @Summary      Export HTML report
// @Description  Renders the flow's current analysis report as a self-contained HTML page and returns it base64-encoded under `data`. Needs a report (409 until the flow was analyzed). On the desktop a `path` also writes the file there; with accounts the path is ignored.
// @Tags         export
// @Param        request body object true "{flowId, path}"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "{status: ok, data: base64 HTML}"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Flow not found"
// @Failure      409 {object} map[string]string "No analysis report"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/export/html [post]
func (h *ExportHandler) handleExportHTML(w http.ResponseWriter, r *http.Request) {
	h.serveDocExport(w, r, "html")
}

// serveDocExport renders the flow's current report as the export format id into the base64
// envelope the document exports share: {"status": "ok", "data": <base64>}.
func (h *ExportHandler) serveDocExport(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Path   string `json:"path"`
		FlowID string `json:"flowId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, service.FlowRead)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	report, _ := h.analysisSvc.CurrentReport(doc)
	if report == nil {
		render.Error(w, fmt.Errorf("no analysis report available — run analysis first"), http.StatusConflict)
		return
	}

	// The server-side file write (path != "") is a desktop-only feature: a
	// multi-tenant cloud user must NOT be able to choose an arbitrary path on
	// the server's filesystem (arbitrary file write — cron injection,
	// authorized_keys, overwriting source). Drop the path in cloud/JWT mode so
	// only the base64 response is produced; the frontend download path uses that
	// base64 via a data URL, never a server path.
	exportPath := req.Path
	if h.security.JWTEnabled {
		exportPath = ""
	}
	content, f, err := h.exportSvc.Export(id, doc, report, exportPath)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowExport, ResourceType: "flow", ResourceID: req.FlowID, OrgID: doc.OrganizationID, Meta: map[string]string{"format": f.ID}})
	render.JSON(w, map[string]any{
		"status": "ok",
		"data":   base64.StdEncoding.EncodeToString(content),
	})
}

// handleSaveClientExport writes a findings export the browser holds (its CSV
// of the filtered list, or the SARIF / JUnit it fetched from /api/analysis/
// export) to the path picked in the desktop native save dialog — the same dialog-then-server-write flow the PDF,
// Markdown and HTML exports use. Desktop/local only: in cloud mode a caller
// must never choose a server path.
// @Summary      Save a client-rendered findings export
// @Description  Desktop only. Writes the findings panel's CSV, or the SARIF / JUnit text it fetched, to the absolute path chosen in the native save dialog (.csv, .sarif or .xml only). 403 in cloud mode.
// @Tags         export
// @Accept       json
// @Produce      json
// @Param        request body object true "{path, content}"
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      403 {object} map[string]string "Forbidden (cloud mode)"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/export/save [post]
func (h *ExportHandler) handleSaveClientExport(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("saving an export to a server path is not available in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := h.exportSvc.SaveClientExport(req.Path, req.Content); err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Export flow as PDF
// @Description  Renders the flow's current analysis report as a PDF and returns it base64-encoded under `data`. Needs a report (409 until the flow was analyzed). On the desktop a `path` also writes the file there; with accounts the path is ignored.
// @Tags         export
// @Param        request body object true "{flowId, path}"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "{status: ok, data: base64 PDF}"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Flow not found"
// @Failure      409 {object} map[string]string "No analysis report"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/export/pdf [post]
func (h *ExportHandler) handleExportPDF(w http.ResponseWriter, r *http.Request) {
	h.serveDocExport(w, r, "pdf")
}

// Mount registers the /api/export group. The document exports render for
// seconds after one authorized read, outside the request transaction; the
// desktop-only compare and save keep it.
func (h *ExportHandler) Mount(rb *route.Builder) {
	rb.Route("/api/export", func(rb *route.Builder) {
		rb.Post("/compare", h.handleCompareCurrentWith)
		rb.Post("/markdown", h.handleExportMarkdown, route.NoTx())
		rb.Post("/html", h.handleExportHTML, route.NoTx())
		rb.Post("/pdf", h.handleExportPDF, route.NoTx())
		rb.Post("/save", h.handleSaveClientExport)
	})
}
