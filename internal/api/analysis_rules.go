package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	"pad-core/analyzer"
	"pad-core/models"
)

// @Summary      List analysis rules
// @Description  Returns all available analysis rules and their current configuration.
// @Tags         analysis
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
// @Param        orgId query string false "Scope the catalog to an organisation the caller belongs to (org custom rules + overrides)"
// @Router       /api/analysis/rules [get]
func (h *AnalysisHandler) handleGetRules(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.catalogOrg(w, r)
	if !ok {
		return
	}
	render.JSON(w, h.analysisSvc.GetRulesFor(r.Context(), orgID))
}

// catalogOrg reads the optional ?orgId= for the rule catalog endpoints and
// checks the caller is a member — org custom rules are tenant data.
func (h *AnalysisHandler) catalogOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	orgID := r.URL.Query().Get("orgId")
	if orgID == "" {
		return "", true
	}
	if !requireOrgMember(w, r, h.security, orgID) {
		return "", false
	}
	return orgID, true
}

// handleValidateCustomRules validates a custom-rules payload without loading
// it — the same checks `bakicli rules test` applies.
// @Summary      Validate custom rules
// @Description  Validates custom-rule JSON without installing it: per-entry validity, errors, and skip warnings.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Results"
// @Router       /api/analysis/custom-rules/validate [post]
func (h *AnalysisHandler) handleValidateCustomRules(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var raw json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}
	// Accept a raw array, a single object, or the {"rules": …} envelope.
	var payload struct {
		Rules json.RawMessage `json:"rules"`
	}
	inner := raw
	if err := json.Unmarshal(raw, &payload); err == nil && len(payload.Rules) > 0 {
		inner = payload.Rules
	}
	var configs []analyzer.CustomRuleConfig
	if err := json.Unmarshal(inner, &configs); err != nil {
		var one analyzer.CustomRuleConfig
		if err2 := json.Unmarshal(inner, &one); err2 != nil || one.ID == "" {
			render.Error(w, fmt.Errorf("not a custom-rule JSON array or object"), http.StatusBadRequest)
			return
		}
		configs = []analyzer.CustomRuleConfig{one}
	}
	type entryResult struct {
		Index  int    `json:"index"`
		ID     string `json:"id"`
		Valid  bool   `json:"valid"`
		Error  string `json:"error,omitempty"`
		Loaded bool   `json:"loaded"`
	}
	out := make([]entryResult, len(configs))
	valid := 0
	for i, cfg := range configs {
		out[i] = entryResult{Index: i, ID: cfg.ID}
		if _, err := analyzer.NewCustomRule(cfg); err != nil {
			out[i].Error = err.Error()
			continue
		}
		out[i].Valid = true
		valid++
	}
	render.JSON(w, map[string]any{"entries": out, "valid": valid, "invalid": len(configs) - valid})
}

// @Summary      Enable/disable analysis rule
// @Description  Enables or disables a specific analysis rule by ID.
// @Tags         analysis
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/analysis/rule/enabled [post]
func (h *AnalysisHandler) handleSetRuleEnabled(w http.ResponseWriter, r *http.Request) {
	// System admin, not member: this writes the DEPLOYMENT-WIDE settings singleton — any org member
	// could otherwise change analysis for every tenant. Per-org configuration lives at /api/orgs/{id}/rules.
	// Kept on the role check, not Can's InstanceAdmin: that would demand the admin scope of a write token.
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	// The frontend sends "ruleId"; "id" kept for compat — decoding only "id" wrote to rule "" silently.
	var req struct {
		ID      string `json:"id"`
		RuleID  string `json:"ruleId"`
		Enabled bool   `json:"enabled"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.RuleID != "" {
		req.ID = req.RuleID
	}
	if req.ID == "" {
		render.Error(w, fmt.Errorf("ruleId is required"), http.StatusBadRequest)
		return
	}
	if err := h.analysisSvc.SetRuleEnabled(r.Context(), req.ID, req.Enabled); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleTestCustomRule runs a CANDIDATE rule against one of the caller's flows and reports what it
// would match — validate answers "does this compile", this "does it do anything". Member-level, stores nothing.
// @Summary      Test a candidate custom rule against a flow
// @Description  Runs one unsaved rule against a flow and returns the findings it would produce.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Matches"
// @Failure      400 {object} map[string]string "Bad Request"
// @Router       /api/analysis/custom-rules/test [post]
func (h *AnalysisHandler) handleTestCustomRule(w http.ResponseWriter, r *http.Request) {
	if !requireInstancePerm(w, r, h.security, service.InstanceWrite) {
		return
	}
	var req struct {
		Rule   analyzer.CustomRuleConfig `json:"rule"`
		FlowID string                    `json:"flowId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Rule.ID == "" {
		render.Error(w, fmt.Errorf("rule.id is required"), http.StatusBadRequest)
		return
	}
	if req.FlowID == "" {
		render.Error(w, fmt.Errorf("flowId is required — pick a flow to test the rule against"), http.StatusBadRequest)
		return
	}

	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, service.FlowRead)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	findings, err := h.analysisSvc.TestRule(r.Context(), doc, req.Rule)
	if err != nil {
		// A rule that does not compile is the AUTHOR's error, not a server fault — same 400 as the save endpoint.
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	render.JSON(w, map[string]any{
		"matches":  len(findings),
		"findings": findings,
		"flowName": doc.Name,
	})
}

// @Summary      Update analysis rule config
// @Description  Updates the configuration parameters for a specific analysis rule.
// @Tags         analysis
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/analysis/rule/config [post]
func (h *AnalysisHandler) handleUpdateRuleConfig(w http.ResponseWriter, r *http.Request) {
	// System admin — same reasoning as handleSetRuleEnabled: deployment-wide settings singleton.
	// Kept on the role check, not Can's InstanceAdmin: that would demand the admin scope of a write token.
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	// Same id/ruleId compatibility shim as handleSetRuleEnabled.
	var req struct {
		ID     string            `json:"id"`
		RuleID string            `json:"ruleId"`
		Config models.RuleConfig `json:"config"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.RuleID != "" {
		req.ID = req.RuleID
	}
	if req.ID == "" {
		render.Error(w, fmt.Errorf("ruleId is required"), http.StatusBadRequest)
		return
	}
	if err := h.analysisSvc.UpdateRuleConfig(r.Context(), req.ID, req.Config); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// registerAnalysisRuleRoutes wires the analyzer-rule and custom-rule
// configuration sub-routes.
func (h *AnalysisHandler) registerAnalysisRuleRoutes(rb *route.Builder) {
	rb.Get("/rules", h.handleGetRules, route.RateLimit(route.RateGeneral))
	rb.Post("/custom-rules/validate", h.handleValidateCustomRules)
	rb.Post("/custom-rules/test", h.handleTestCustomRule)
	rb.Post("/rule/enabled", h.handleSetRuleEnabled)
	rb.Post("/rule/config", h.handleUpdateRuleConfig)
}
