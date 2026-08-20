package api

import (
	"fmt"
	"net/http"

	"pad-core/analyzer"
	"pad-core/models"

	"pad-analyzer/internal/api/render"
)

// handleEvaluatePolicy analyzes a flow and gates the result against a policy
// supplied in the request body (a named rule set + pass/fail GateSeverity),
// returning the PolicyResult (passed + violations). This is the server-side
// counterpart of `bakicli -policy`.
// @Summary      Evaluate a flow against an inline policy
// @Tags         policy
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/policy/evaluate [post]
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

// ---- Persistent policy CRUD (cloud mode only) ----

func (h *AnalysisHandler) policyAvailable(w http.ResponseWriter) bool {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("policies require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return false
	}
	return true
}

// @Summary      Save a named policy for an organization
// @Tags         policy
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/policy/save [post]
func (h *AnalysisHandler) handleSavePolicy(w http.ResponseWriter, r *http.Request) {
	if !h.policyAvailable(w) {
		return
	}
	var p models.Policy
	if !decodeBody(w, r, &p) {
		return
	}
	if p.Name == "" {
		render.Error(w, fmt.Errorf("policy name is required"), http.StatusBadRequest)
		return
	}
	// Saving a policy is a WRITE to the org-wide CI gate (SavePolicy upserts on
	// id+org_id), so it must require admin — a viewer/guest must not be able to
	// rewrite or overwrite the org's governance rules.
	if !requireOrgAdmin(w, r, h.security, p.OrgID) {
		return
	}
	if err := h.backend.SavePolicy(r.Context(), &p); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, p)
}

// @Summary      List policies for an organization
// @Tags         policy
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/policy/list [post]
func (h *AnalysisHandler) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	if !h.policyAvailable(w) {
		return
	}
	var req struct {
		OrgID string `json:"orgId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if !requireOrgMember(w, r, h.security, req.OrgID) {
		return
	}
	policies, err := h.backend.ListPolicies(r.Context(), req.OrgID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, policies)
}

// @Summary      Get a single policy by ID
// @Tags         policy
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/policy/get [post]
func (h *AnalysisHandler) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	if !h.policyAvailable(w) {
		return
	}
	var req struct {
		OrgID string `json:"orgId"`
		ID    string `json:"id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !requireOrgMember(w, r, h.security, req.OrgID) {
		return
	}
	p, err := h.backend.GetPolicy(r.Context(), req.OrgID, req.ID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, p)
}

// @Summary      Delete a policy
// @Tags         policy
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/policy/delete [post]
func (h *AnalysisHandler) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	if !h.policyAvailable(w) {
		return
	}
	var req struct {
		OrgID string `json:"orgId"`
		ID    string `json:"id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	// Deleting a policy removes an org-wide CI gate; restrict to admins so a
	// low-privileged member can't disable the org's governance gate.
	if !requireOrgAdmin(w, r, h.security, req.OrgID) {
		return
	}
	if err := h.backend.DeletePolicy(r.Context(), req.OrgID, req.ID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleEvaluatePolicyByID resolves a persisted policy by org+id, then
// evaluates it against a flow — the server-side counterpart of `bakicli
// -policy` but using a stored policy instead of an inline one.
// @Summary      Evaluate a flow against a persisted policy
// @Tags         policy
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/policy/evaluate-by-id [post]
func (h *AnalysisHandler) handleEvaluatePolicyByID(w http.ResponseWriter, r *http.Request) {
	if !h.policyAvailable(w) {
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
		OrgID  string `json:"orgId"`
		ID     string `json:"id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !requireOrgMember(w, r, h.security, req.OrgID) {
		return
	}
	policy, err := h.backend.GetPolicy(r.Context(), req.OrgID, req.ID)
	if err != nil {
		render.Error(w, err, 0)
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
	render.JSON(w, analyzer.EvaluatePolicy(report, *policy))
}
