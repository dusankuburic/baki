package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/service"
	"pad-core/analyzer"
	"pad-core/models"
)

// @Summary      List analysis rules
// @Description  Returns all available analysis rules and their current configuration, with each rule's stage (stable, preview), introducedIn and kind (block, flow, library; library rules are listed only where desktop folder analysis runs them).
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
// it — the same strict checks a save and `bakicli rules test` apply, per entry.
// @Summary      Validate custom rules
// @Description  Validates custom-rule JSON (an array, one rule with an id, or {"rules": either}) strictly, without installing it. Each entry reports valid, its first error with field and reason, and every problem (problems; warning: true for one the rule runs with). A malformed document is a 400.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Results"
// @Failure      400 {object} render.ErrorResponse "Not a custom-rule document"
// @Router       /api/analysis/custom-rules/validate [post]
func (h *AnalysisHandler) handleValidateCustomRules(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var raw json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}
	entries, err := analyzer.DecodeCustomRuleDocument(raw)
	if err != nil {
		render.Error(w, apperr.Invalid("", err.Error()).WithDetails(map[string]any{"reason": analyzer.ReasonInvalidJSON}), 0)
		return
	}
	type entryResult struct {
		Index    int                    `json:"index"`
		ID       string                 `json:"id"`
		Valid    bool                   `json:"valid"`
		Error    string                 `json:"error,omitempty"`
		Field    string                 `json:"field,omitempty"`
		Reason   string                 `json:"reason,omitempty"`
		Problems []customRuleProblemOut `json:"problems,omitempty"`
		Loaded   bool                   `json:"loaded"`
	}
	out := make([]entryResult, len(entries))
	valid := 0
	for i, e := range entries {
		problems := []*analyzer.CustomRuleError{e.Err}
		if e.Err == nil {
			problems = analyzer.CheckCustomRule(e.Config, analyzer.CustomRuleStrict)
		}
		out[i] = entryResult{Index: e.Index, ID: e.Config.ID, Valid: true, Problems: toCustomRuleProblems(problems)}
		if e.Err != nil {
			out[i].ID = e.Err.RuleID
		}
		for _, p := range problems {
			if !p.Warning {
				out[i].Valid, out[i].Error, out[i].Field, out[i].Reason = false, p.Error(), p.Field, p.Reason
				break
			}
		}
		if out[i].Valid {
			valid++
		}
	}
	render.JSON(w, map[string]any{"entries": out, "valid": valid, "invalid": len(entries) - valid})
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
// @Description  Runs one unsaved rule, checked strictly, against a flow and returns the findings it would produce. The body is {rule, flowId} and nothing else; a rule the strict check refuses is a 400 with details.field ("rule.<field>") and details.reason.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Matches"
// @Failure      400 {object} render.ErrorResponse "Bad Request"
// @Router       /api/analysis/custom-rules/test [post]
func (h *AnalysisHandler) handleTestCustomRule(w http.ResponseWriter, r *http.Request) {
	if !requireInstancePerm(w, r, h.security, service.InstanceWrite) {
		return
	}
	var req struct {
		Rule   analyzer.CustomRuleConfig `json:"rule"`
		FlowID string                    `json:"flowId"`
	}
	if err := decodeStrictJSON(r.Body, &req); err != nil {
		render.Error(w, err, 0)
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
		// A rule that does not compile is the AUTHOR's error, not a server fault — same 400 as the save endpoint,
		// with details.field "rule.<field>".
		render.Error(w, customRuleError(err, "rule."), http.StatusBadRequest)
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
	// The engine reads an unknown severity as error; refuse one here, where an
	// admin sees the answer.
	if sev := models.Severity(strings.ToLower(strings.TrimSpace(req.Config.Severity))); sev != "" {
		if !sev.Valid() {
			render.Error(w, fmt.Errorf("config.severity must be one of error, warning, info"), http.StatusBadRequest)
			return
		}
		req.Config.Severity = string(sev)
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
