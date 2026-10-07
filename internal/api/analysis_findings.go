package api

import (
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/service"
	"pad-core/analyzer"
	"pad-core/models"
)

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

	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, service.FlowRead)
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

	doc, err := h.flowSvc.GetAuthorized(r.Context(), flowID, service.FlowRead)
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

	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, service.FlowRead)
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

	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, service.FlowRead)
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

	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, service.FlowRead)
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

// @Summary      Subflow dependency graph
// @Tags         analysis
// @Produce      json
// @Success      200 {object} map[string]interface{} "Dependencies"
// @Router       /api/analysis/dependencies [get]
func (h *AnalysisHandler) handleGetDependencies(w http.ResponseWriter, r *http.Request) {
	result := h.analysisSvc.GetDependencyAnalysis()
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

	docA, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowAID, service.FlowRead)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	docB, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowBID, service.FlowRead)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	result := h.analysisSvc.CompareFlows(docA, docB)
	render.JSON(w, result)
}

// registerFindingRoutes wires the analysis findings-introspection sub-routes
// (lineage, graphs, dataflow, diff/history, dedupe, related, compare). The
// analyses burn seconds of CPU per flow: outside the request transaction.
func (h *AnalysisHandler) registerFindingRoutes(rb *route.Builder) {
	rb.Post("/lineage", h.handleGetVariableLineage, route.NoTx())
	rb.Post("/graph", h.handleGetExecutionGraph, route.NoTx())
	rb.Post("/dataflow", h.handleGetDataFlow, route.NoTx())
	rb.Post("/diff", h.handleDiff, route.NoTx())
	rb.Post("/history", h.handleGetHistory)
	rb.Get("/dependencies", h.handleGetDependencies, route.RateLimit(route.RateGeneral))
	rb.Post("/deduplicate", h.handleDeduplicate, route.NoTx())
	rb.Post("/related", h.handleRelatedFindings, route.NoTx())
	rb.Post("/compare", h.handleCompareFlows, route.NoTx())
}
