package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"pad-analyzer/internal/analyzer"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type AnalysisHandler struct {
	analysisSvc *service.AnalysisService
	flowSvc     *service.FlowService
	security    *SecurityConfig
	backend     storageif.StorageBackend
}

func NewAnalysisHandler(analysisSvc *service.AnalysisService, flowSvc *service.FlowService, backend storageif.StorageBackend, security *SecurityConfig) *AnalysisHandler {
	return &AnalysisHandler{
		analysisSvc: analysisSvc,
		flowSvc:     flowSvc,
		security:    security,
		backend:     backend,
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
	logAudit(r.Context(), h.backend, r, AuditActionFlowAnalyze, "flow", req.FlowID, nil)
	render.JSON(w, res)
}

func (h *AnalysisHandler) handleGetVariableLineage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Variable string `json:"varName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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
	var req struct {
		FolderPath string `json:"folderPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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
	w.Write([]byte(html))
}

func (h *AnalysisHandler) handleGetDependencies(w http.ResponseWriter, r *http.Request) {
	result := h.analysisSvc.GetDependencyAnalysis()
	render.JSON(w, result)
}

func (h *AnalysisHandler) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
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
		"deduplicated": deduped,
		"groups":       groups,
		"originalCount": len(report.Findings),
		"dedupedCount": len(deduped),
	})
}

func (h *AnalysisHandler) handleRelatedFindings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID  string `json:"flowId"`
		BlockID string `json:"blockId"`
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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
