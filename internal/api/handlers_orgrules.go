package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"pad-analyzer/internal/api/render"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
)

// Per-org custom analyzer rules (R4).
//
// Reads require org membership; writes require org admin — the same split as
// org channels. Analysis configuration decides what a team is told about its
// own flows, so letting any member rewrite it is the multi-tenancy defect this
// feature exists to close: before R4, rule enable/severity lived in the
// DEPLOYMENT settings singleton behind a plain RoleMember check, so one
// member's toggle silently changed analysis for every tenant.

// orgCustomRuleOut is the wire shape. Config is echoed back parsed rather than
// as a raw blob so the editor round-trips a typed object.
type orgCustomRuleOut struct {
	ID        string                    `json:"id"`
	RuleID    string                    `json:"ruleId"`
	Config    analyzer.CustomRuleConfig `json:"config"`
	Enabled   bool                      `json:"enabled"`
	CreatedAt time.Time                 `json:"createdAt"`
	UpdatedAt time.Time                 `json:"updatedAt"`
}

func toOrgRuleOut(r *storageif.OrgCustomRule) orgCustomRuleOut {
	out := orgCustomRuleOut{
		ID: r.ID, RuleID: r.RuleID, Enabled: r.Enabled,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	// A stored row that no longer parses is surfaced as an empty config rather
	// than dropped: the admin needs to SEE the broken rule to delete it. The
	// analysis path skips it (with a warning) either way.
	_ = json.Unmarshal(r.Config, &out.Config)
	return out
}

// @Summary      List an org's custom analyzer rules
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {array} object "Rules"
// @Router       /api/orgs/{id}/rules [get]
func (h *OrgHandler) handleOrgRuleList(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("org custom rules require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	org := h.requireMember(w, r)
	if org == nil {
		return
	}
	rules, err := h.backend.ListOrgCustomRules(r.Context(), org.ID, false)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	out := make([]orgCustomRuleOut, len(rules))
	for i, rule := range rules {
		out[i] = toOrgRuleOut(rule)
	}
	render.JSON(w, out)
}

// @Summary      Create or update an org custom analyzer rule
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       json
// @Produce      json
// @Success      200 {object} object "Rule"
// @Router       /api/orgs/{id}/rules [post]
func (h *OrgHandler) handleOrgRuleSave(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("org custom rules require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	var req struct {
		ID      string                    `json:"id"`
		Config  analyzer.CustomRuleConfig `json:"config"`
		Enabled *bool                     `json:"enabled"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Config.ID == "" {
		render.Error(w, fmt.Errorf("config.id is required — it is the rule id that appears on findings"), http.StatusBadRequest)
		return
	}
	// Compile before storing. This is the SAME construction path
	// /api/analysis/custom-rules/validate reports on and that the analysis
	// resolver uses, so the editor's preview, this endpoint, and the analyzer
	// cannot disagree about whether a rule is valid.
	if _, err := analyzer.NewCustomRule(req.Config); err != nil {
		render.Error(w, fmt.Errorf("invalid rule: %w", err), http.StatusBadRequest)
		return
	}
	raw, err := json.Marshal(req.Config)
	if err != nil {
		render.Error(w, fmt.Errorf("encode rule: %w", err), http.StatusBadRequest)
		return
	}

	id := req.ID
	if id == "" {
		id = uuid.NewString()
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rule := &storageif.OrgCustomRule{
		ID: id, OrgID: org.ID, RuleID: req.Config.ID,
		Config: raw, Enabled: enabled,
	}
	if err := h.backend.SaveOrgCustomRule(r.Context(), rule); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.invalidateRuleProfile(org.ID)
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, "org_rule_save", "org", org.ID,
		map[string]string{"ruleId": req.Config.ID})
	render.JSON(w, toOrgRuleOut(rule))
}

// @Summary      Delete an org custom analyzer rule
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        ruleId path string true "Rule row ID"
// @Produce      json
// @Success      200 {object} map[string]string "Deleted"
// @Router       /api/orgs/{id}/rules/{ruleId} [delete]
func (h *OrgHandler) handleOrgRuleDelete(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("org custom rules require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	if err := h.backend.DeleteOrgCustomRule(r.Context(), org.ID, chi.URLParam(r, "ruleId")); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.invalidateRuleProfile(org.ID)
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, "org_rule_delete", "org", org.ID,
		map[string]string{"ruleId": chi.URLParam(r, "ruleId")})
	render.JSON(w, map[string]string{"status": "ok"})
}

// invalidateRuleProfile drops the org's cached rule profile so the next
// analysis re-resolves. Without it a rule change would not take effect until
// the resolver's TTL elapsed — long enough for an admin to save a rule, re-run
// analysis, and conclude it does not work.
func (h *OrgHandler) invalidateRuleProfile(orgID string) {
	if h.ruleProfiles != nil {
		h.ruleProfiles.Invalidate(orgID)
	}
}

// orgRuleExport is a `.bakirc.json`-shaped document: the org's per-rule profile
// plus its custom rules, INLINE.
//
// The field names and shapes mirror cmd/bakicli's bakiConfig exactly, so a
// pipeline can do
//
//	curl -H "Authorization: Bearer $PAT" .../api/orgs/$ORG/rules/export > .bakirc.json
//	bakicli -format sarif ./flows
//
// and gate on the same rules the team sees in the UI. Two artifacts (a profile
// and a separate rules file) would be free to drift apart, and a CI gate quietly
// enforcing a different rule set than the team configured is the failure mode
// B6 was about.
type orgRuleExport struct {
	RuleConfig        map[string]orgRuleExportConf `json:"ruleConfig,omitempty"`
	CustomRulesInline []analyzer.CustomRuleConfig  `json:"customRulesInline,omitempty"`
}

// orgRuleExportConf mirrors bakiRuleConf. Enabled is a POINTER for the same
// reason it is there: the CLI treats an absent `enabled` as "leave at default",
// so emitting a bare false for every unset rule would silently disable rules the
// org never touched.
type orgRuleExportConf struct {
	Enabled  *bool  `json:"enabled,omitempty"`
	Severity string `json:"severity,omitempty"`
}

// @Summary      Export an org's analysis configuration for CI
// @Description  Returns the org's rule profile + custom rules as a .bakirc.json document consumable by bakicli.
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} object "Config"
// @Router       /api/orgs/{id}/rules/export [get]
func (h *OrgHandler) handleOrgRuleExport(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("org custom rules require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	// Member, not admin: a CI pipeline runs as a service account that needs to
	// READ the configuration it gates on, and reading a rule set discloses
	// nothing an org member cannot already see in the UI.
	org := h.requireMember(w, r)
	if org == nil {
		return
	}

	out := orgRuleExport{}

	if settings, err := h.backend.LoadOrgSettings(r.Context(), org.ID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	} else if settings != nil && len(settings.Analysis.Rules) > 0 {
		out.RuleConfig = make(map[string]orgRuleExportConf, len(settings.Analysis.Rules))
		for id, rc := range settings.Analysis.Rules {
			enabled := rc.Enabled
			out.RuleConfig[id] = orgRuleExportConf{Enabled: &enabled, Severity: rc.Severity}
		}
	}

	// enabledOnly: a paused rule must not gate CI. This mirrors what the
	// analysis path compiles, so the export and the server agree.
	rules, err := h.backend.ListOrgCustomRules(r.Context(), org.ID, true)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	for _, stored := range rules {
		var cfg analyzer.CustomRuleConfig
		if err := json.Unmarshal(stored.Config, &cfg); err != nil {
			// Refuse rather than emit a partial config. A CI gate silently
			// missing one of the org's rules is exactly the "gate that cannot
			// fail" shape; the admin needs to fix or delete the broken rule.
			render.Error(w, fmt.Errorf("rule %q is stored in an unreadable form; delete and re-create it", stored.RuleID), http.StatusConflict)
			return
		}
		out.CustomRulesInline = append(out.CustomRulesInline, cfg)
	}

	w.Header().Set("Content-Disposition", `attachment; filename=".bakirc.json"`)
	render.JSON(w, out)
}
