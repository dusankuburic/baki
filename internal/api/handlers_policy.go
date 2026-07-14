package api

import (
	"net/http"

	"pad-core/analyzer"
	"pad-core/models"

	"pad-analyzer/internal/api/render"
)

// handleEvaluatePolicy analyzes a flow and gates the result against a policy
// supplied in the request body (a named rule set + pass/fail GateSeverity),
// returning the PolicyResult (passed + violations). This is the server-side
// counterpart of `bakicli -policy`. The policy is evaluated as given; persisted,
// org-assigned policies are a separate (future) storage concern.
func (h *AnalysisHandler) handleEvaluatePolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID string        `json:"flowId"`
		Policy models.Policy `json:"policy"`
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

	render.JSON(w, analyzer.EvaluatePolicy(report, req.Policy))
}
