package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
)

// Per-org custom analyzer rules (R4): reads require org membership, writes org admin. Analysis
// configuration decides what a team is told about its own flows — before R4, rule enable/severity
// lived in the DEPLOYMENT settings singleton behind a plain RoleMember check, so one member's
// toggle silently changed analysis for every tenant.

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
	// A stored row that no longer parses is surfaced as an empty config, not dropped: the admin
	// needs to SEE the broken rule to delete it (the analysis path skips it either way).
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
	rules, err := h.orgRules.List(r.Context(), chi.URLParam(r, "id"), false)
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
	orgID := chi.URLParam(r, "id")
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
	// Compile before storing — the SAME construction path /api/analysis/custom-rules/validate
	// reports on and the analysis resolver uses, so preview, this endpoint and the analyzer agree.
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
		ID: id, OrgID: orgID, RuleID: req.Config.ID,
		Config: raw, Enabled: enabled,
	}
	if err := h.orgRules.Save(r.Context(), rule); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionOrgRuleSave, orgID, map[string]string{"ruleId": req.Config.ID}))
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
	orgID := chi.URLParam(r, "id")
	if err := h.orgRules.Delete(r.Context(), orgID, chi.URLParam(r, "ruleId")); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionOrgRuleDelete, orgID, map[string]string{"ruleId": chi.URLParam(r, "ruleId")}))
	render.JSON(w, map[string]string{"status": "ok"})
}

// orgRuleExport is a `.bakirc.json`-shaped document: the org's per-rule profile plus its custom
// rules, INLINE. The field names and shapes mirror cmd/bakicli's bakiConfig exactly, so a pipeline
// can `curl .../rules/export > .bakirc.json` then `bakicli -format sarif ./flows` and gate on the
// same rules the team sees in the UI — two artifacts would drift apart, and a CI gate quietly
// enforcing a different rule set is the B6 failure mode.
type orgRuleExport struct {
	RuleConfig        map[string]orgRuleExportConf `json:"ruleConfig,omitempty"`
	CustomRulesInline []analyzer.CustomRuleConfig  `json:"customRulesInline,omitempty"`
}

// orgRuleExportConf mirrors bakiRuleConf; Enabled is a POINTER because the CLI treats absent as
// "leave at default" — emitting a bare false would silently disable rules the org never touched.
type orgRuleExportConf struct {
	Enabled  *bool  `json:"enabled,omitempty"`
	Severity string `json:"severity,omitempty"`
	// Options: the rule's thresholds — the export used to omit them, so a team that tightened
	// maxDepth in its org settings saw CI gate on the default.
	Options map[string]any `json:"options,omitempty"`
}

// @Summary      Export an org's analysis configuration for CI
// @Description  Returns the org's rule profile + custom rules as a .bakirc.json document consumable by bakicli.
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} object "Config"
// @Router       /api/orgs/{id}/rules/export [get]
func (h *OrgHandler) handleOrgRuleExport(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "id")
	out := orgRuleExport{}

	if settings, err := h.orgRules.OrgSettings(r.Context(), orgID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	} else if settings != nil && len(settings.Analysis.Rules) > 0 {
		out.RuleConfig = make(map[string]orgRuleExportConf, len(settings.Analysis.Rules))
		for id, rc := range settings.Analysis.Rules {
			enabled := rc.Enabled
			out.RuleConfig[id] = orgRuleExportConf{Enabled: &enabled, Severity: rc.Severity, Options: rc.Options}
		}
	}

	// enabledOnly: a paused rule must not gate CI. This mirrors what the
	// analysis path compiles, so the export and the server agree.
	rules, err := h.orgRules.List(r.Context(), orgID, true)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	for _, stored := range rules {
		var cfg analyzer.CustomRuleConfig
		if err := json.Unmarshal(stored.Config, &cfg); err != nil {
			// Refuse rather than emit a partial config: a CI gate silently missing one of the
			// org's rules is exactly the "gate that cannot fail" shape; the admin must fix it.
			render.Error(w, fmt.Errorf("rule %q is stored in an unreadable form; delete and re-create it", stored.RuleID), http.StatusConflict)
			return
		}
		out.CustomRulesInline = append(out.CustomRulesInline, cfg)
	}

	w.Header().Set("Content-Disposition", `attachment; filename=".bakirc.json"`)
	render.JSON(w, out)
}

// registerRuleRoutes wires the per-org custom analyzer rules sub-routes, which live in the server store.
func (h *OrgHandler) registerRuleRoutes(rb *route.Builder) {
	// Per-org custom analyzer rules. Reads: member. Writes: admin.
	rb.Route("/rules", func(rb *route.Builder) {
		// Member, not admin, for the export too: a CI pipeline runs as a service account that needs to READ
		// the configuration it gates on, and reading a rule set discloses nothing a member cannot see.
		read := rb.With(h.requireOrg(service.OrgRead))
		read.Get("/", h.handleOrgRuleList)
		read.Get("/export", h.handleOrgRuleExport)
		admin := rb.With(h.requireOrg(service.OrgAdmin))
		admin.Post("/", h.handleOrgRuleSave, route.Idempotent())
		admin.Delete("/{ruleId}", h.handleOrgRuleDelete)
	}, route.Needs(capability.ServerStore))
}
