package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/service"
)

type ExportHandler struct {
	exportSvc  *service.ExportService
	flowSvc    *service.FlowService
	analysisSvc *service.AnalysisService
	security   *SecurityConfig
}

func NewExportHandler(exportSvc *service.ExportService, flowSvc *service.FlowService, analysisSvc *service.AnalysisService, security *SecurityConfig) *ExportHandler {
	return &ExportHandler{exportSvc: exportSvc, flowSvc: flowSvc, analysisSvc: analysisSvc, security: security}
}

func (h *ExportHandler) handleCompareCurrentWith(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	var req struct {
		Path   string `json:"path"`
		FlowID string `json:"flowId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	var req struct {
		Path   string `json:"path"`
		FlowID string `json:"flowId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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

	content, err := h.exportSvc.ExportMarkdown(doc, report, req.Path)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{
		"status":  "ok",
		"content": string(content),
	})
}

func (h *ExportHandler) handleExportPDF(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	var req struct {
		Path   string `json:"path"`
		FlowID string `json:"flowId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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

	content, err := h.exportSvc.ExportPDF(doc, report, req.Path)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]any{
		"status": "ok",
		"data":   base64.StdEncoding.EncodeToString(content),
	})
}
