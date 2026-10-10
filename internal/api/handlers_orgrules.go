package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/service"
	"pad-core/analyzer"
)

// Per-org custom analyzer rules (R4): reads require org membership, writes org admin. Analysis
// configuration decides what a team is told about its own flows — before R4, rule enable/severity
// lived in the DEPLOYMENT settings singleton behind a plain RoleMember check, so one member's
// toggle silently changed analysis for every tenant.

// orgCustomRuleOut is the wire shape. Config is echoed back parsed rather than
// as a raw blob so the editor round-trips a typed object: the stored config's
// known fields with kind and schemaVersion spelled out, never its unknown ones,
// so the echo is always something a strict save accepts (the save keeps the
// stored unknown fields itself).
type orgCustomRuleOut struct {
	ID        string                    `json:"id"`
	RuleID    string                    `json:"ruleId"`
	Config    analyzer.CustomRuleConfig `json:"config"`
	Enabled   bool                      `json:"enabled"`
	CreatedAt time.Time                 `json:"createdAt"`
	UpdatedAt time.Time                 `json:"updatedAt"`
	// Problem says why the analysis skips the rule (ProblemReason is the analyzer's reason code): it does not
	// decode, a newer release wrote it (UNKNOWN_KIND, UNSUPPORTED_VERSION), or a value no longer compiles. The admin
	// needs to SEE the broken rule to fix or delete it.
	Problem       string `json:"problem,omitempty"`
	ProblemReason string `json:"problemReason,omitempty"`
}

func toOrgRuleOut(r service.OrgRule) orgCustomRuleOut {
	out := orgCustomRuleOut{
		ID: r.Row.ID, RuleID: r.Row.RuleID, Config: r.Config.Normalized(), Enabled: r.Row.Enabled,
		CreatedAt: r.Row.CreatedAt, UpdatedAt: r.Row.UpdatedAt,
	}
	if r.Problem != nil {
		out.Problem, out.ProblemReason = r.Problem.Error(), r.Problem.Reason
	}
	return out
}

type orgRulesRequest struct {
	OrgID string `path:"id"`
}

// @Summary      List an org's custom analyzer rules
// @Description  Each rule's config as stored (kind and schemaVersion spelled out); problem says why the analysis skips a rule.
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {array} object "Rules"
// @Router       /api/orgs/{id}/rules [get]
func (h *OrgHandler) listOrgRules(ctx context.Context, req orgRulesRequest) ([]orgCustomRuleOut, error) {
	rules, err := h.orgRules.List(ctx, req.OrgID, false)
	if err != nil {
		return nil, err
	}
	out := make([]orgCustomRuleOut, len(rules))
	for i, rule := range rules {
		out[i] = toOrgRuleOut(rule)
	}
	return out, nil
}

// saveOrgRuleRequest is a full save ({id?, config, enabled?}) or, without config, a toggle ({id, enabled}).
type saveOrgRuleRequest struct {
	OrgID   string                     `json:"-" path:"id"`
	ID      string                     `json:"id"`
	Config  *analyzer.CustomRuleConfig `json:"config"`
	Enabled *bool                      `json:"enabled"`
}

// @Summary      Create or update an org custom analyzer rule
// @Description  With config: the rule is checked strictly (an unknown field, a missing id or a built-in rule's id is a 400 with details.field and details.reason) and stored. Without config: pauses or resumes the stored rule {id} (enabled required) and leaves its config as stored. A rule's id cannot change (409).
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       json
// @Produce      json
// @Success      200 {object} object "Rule"
// @Failure      400 {object} render.ErrorResponse "Invalid rule"
// @Failure      404 {object} render.ErrorResponse "No such rule"
// @Failure      409 {object} render.ErrorResponse "Rule id changed, or the stored rule is from a newer version"
// @Router       /api/orgs/{id}/rules [post]
func (h *OrgHandler) saveOrgRule(ctx context.Context, req saveOrgRuleRequest) (orgCustomRuleOut, error) {
	saved, err := h.orgRules.SaveRule(ctx, req.OrgID, service.OrgRuleSave{RowID: req.ID, Config: req.Config, Enabled: req.Enabled})
	if err != nil {
		return orgCustomRuleOut{}, customRuleError(err, "config.")
	}
	meta := map[string]string{"ruleId": saved.Row.RuleID}
	if req.Config == nil {
		meta["enabled"] = strconv.FormatBool(saved.Row.Enabled)
	}
	h.audit.Record(ctx, orgAuditEntry(audit.ActionOrgRuleSave, req.OrgID, meta))
	return toOrgRuleOut(saved), nil
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
// enforcing a different rule set is the B6 failure mode. Each rule is its write encoding
// (analyzer.EncodeCustomRule: kind and schemaVersion first, a newer release's fields kept), plus
// "problem" on a rule the server skips.
type orgRuleExport struct {
	RuleConfig        map[string]orgRuleExportConf `json:"ruleConfig,omitempty"`
	CustomRulesInline []json.RawMessage            `json:"customRulesInline,omitempty"`
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

// exportOrgRule is one rule of the export (service.OrgRule.Encoded: a newer release's rule whose fields this build
// cannot decode is emitted as stored). A rule the server skips because a value no longer compiles, or because a
// newer release wrote it, is still emitted, with "problem": bakicli then skips it with a warning, or refuses a kind or
// version it cannot run, instead of the export silently dropping a rule the org believes CI enforces.
func exportOrgRule(r service.OrgRule) (json.RawMessage, error) {
	raw, err := r.Encoded()
	if err != nil || r.Problem == nil {
		return raw, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, err
	}
	if _, ok := keys["problem"]; ok {
		return raw, nil
	}
	problem, err := json.Marshal(r.Problem.Error())
	if err != nil {
		return nil, err
	}
	out := raw[: len(raw)-1 : len(raw)-1]
	if len(keys) > 0 {
		out = append(out, ',')
	}
	out = append(out, `"problem":`...)
	out = append(out, problem...)
	return append(out, '}'), nil
}

// @Summary      Export an org's analysis configuration for CI
// @Description  Returns the org's rule profile + custom rules as a .bakirc.json document consumable by bakicli. A rule the server skips carries "problem"; a rule whose stored JSON does not decode, unless a newer release wrote it, makes the export a 409.
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} object "Config"
// @Failure      409 {object} render.ErrorResponse "A stored rule is unreadable"
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
	for _, rule := range rules {
		if rule.Problem != nil && rule.Problem.Reason == analyzer.ReasonInvalidJSON {
			// Refuse rather than emit a partial config: a CI gate silently missing one of the
			// org's rules is exactly the "gate that cannot fail" shape; the admin must fix it.
			render.Error(w, apperr.Conflict("", fmt.Sprintf("rule %q is stored in an unreadable form; edit and save it, or delete it", rule.Row.RuleID)).
				WithDetails(map[string]any{"ruleId": rule.Row.RuleID, "reason": rule.Problem.Reason}), 0)
			return
		}
		raw, err := exportOrgRule(rule)
		if err != nil {
			render.Error(w, fmt.Errorf("encode rule %q: %w", rule.Row.RuleID, err), http.StatusInternalServerError)
			return
		}
		out.CustomRulesInline = append(out.CustomRulesInline, raw)
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
		read.Get("/", Query(h.listOrgRules))
		read.Get("/export", h.handleOrgRuleExport)
		admin := rb.With(h.requireOrg(service.OrgAdmin))
		admin.Post("/", JSON(h.saveOrgRule), route.Idempotent())
		admin.Delete("/{ruleId}", h.handleOrgRuleDelete)
	}, route.Needs(capability.ServerStore))
}
