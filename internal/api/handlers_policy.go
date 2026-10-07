package api

import (
	"fmt"
	"net/http"

	"pad-core/analyzer"
	"pad-core/models"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/service"
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

	render.JSON(w, analyzer.EvaluatePolicy(report, req.Policy))
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
	var p models.Policy
	if !decodeBody(w, r, &p) {
		return
	}
	if p.Name == "" {
		render.Error(w, fmt.Errorf("policy name is required"), http.StatusBadRequest)
		return
	}
	// Saving a policy is a WRITE to the org-wide CI gate (SavePolicy upserts on id+org_id), so it
	// must require admin — a viewer/guest must not rewrite the org's governance rules.
	if !requireOrgAdmin(w, r, h.security, p.OrgID) {
		return
	}
	if err := h.policies.Save(r.Context(), &p); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionPolicySave, ResourceType: "policy", ResourceID: p.ID, OrgID: p.OrgID, Meta: map[string]string{"orgId": p.OrgID, "name": p.Name}})
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
	policies, err := h.policies.List(r.Context(), req.OrgID)
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
	p, err := h.policies.Get(r.Context(), req.OrgID, req.ID)
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
	if err := h.policies.Delete(r.Context(), req.OrgID, req.ID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionPolicyDelete, ResourceType: "policy", ResourceID: req.ID, OrgID: req.OrgID, Meta: map[string]string{"orgId": req.OrgID}})
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
	policy, err := h.policies.Get(r.Context(), req.OrgID, req.ID)
	if err != nil {
		render.Error(w, err, 0)
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
	render.JSON(w, analyzer.EvaluatePolicy(report, *policy))
}

// registerPolicyRoutes wires the policy-gate sub-routes (evaluate inline or
// a persisted named policy; persistence needs the server-side store).
func (h *AnalysisHandler) registerPolicyRoutes(rb *route.Builder) {
	rb.Post("/policy/evaluate", h.handleEvaluatePolicy) // gate a flow against an inline policy
	rb.Group(func(rb *route.Builder) {
		rb.Post("/policy/save", h.handleSavePolicy)                   // persist a named policy
		rb.Post("/policy/list", h.handleListPolicies)                 // list org's policies
		rb.Post("/policy/get", h.handleGetPolicy)                     // get a single policy
		rb.Post("/policy/delete", h.handleDeletePolicy)               // delete a policy
		rb.Post("/policy/evaluate-by-id", h.handleEvaluatePolicyByID) // gate a flow against a persisted policy
	}, route.Needs(capability.ServerStore))
}
