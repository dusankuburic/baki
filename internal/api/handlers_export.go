package api

import (
	"encoding/base64"
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/service"
)

type ExportHandler struct {
	exportSvc   *service.ExportService
	flowSvc     *service.FlowService
	analysisSvc *service.AnalysisService
	security    *SecurityConfig
}

func NewExportHandler(exportSvc *service.ExportService, flowSvc *service.FlowService, analysisSvc *service.AnalysisService, security *SecurityConfig) *ExportHandler {
	return &ExportHandler{exportSvc: exportSvc, flowSvc: flowSvc, analysisSvc: analysisSvc, security: security}
}

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

	userID := h.security.CallerID(r)
	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "viewer")
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

func (h *ExportHandler) handleExportMarkdown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		FlowID string `json:"flowId"`
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
	content, err := h.exportSvc.ExportMarkdown(doc, report, exportPath)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Return base64 under `data`, mirroring PDF, so the web download path
	// (data:...;base64,res.data in export.ts) works consistently.
	render.JSON(w, map[string]any{
		"status": "ok",
		"data":   base64.StdEncoding.EncodeToString(content),
	})
}

func (h *ExportHandler) handleExportPDF(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		FlowID string `json:"flowId"`
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

	report, _ := h.analysisSvc.CurrentReport(doc)
	if report == nil {
		render.Error(w, fmt.Errorf("no analysis report available — run analysis first"), http.StatusConflict)
		return
	}

	// See handleExportMarkdown: the server-side file write is desktop-only.
	exportPath := req.Path
	if h.security.JWTEnabled {
		exportPath = ""
	}
	content, err := h.exportSvc.ExportPDF(doc, report, exportPath)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]any{
		"status": "ok",
		"data":   base64.StdEncoding.EncodeToString(content),
	})
}
