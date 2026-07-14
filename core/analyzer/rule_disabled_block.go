package analyzer

import (
	"strings"

	"pad-core/models"
)

type DisabledBlockRule struct{}

func (r *DisabledBlockRule) ID() string   { return "disabled-block" }
func (r *DisabledBlockRule) Name() string { return "Disabled block" }
func (r *DisabledBlockRule) Description() string {
	return "Disabled blocks left in the flow should be removed or re-enabled before production."
}
func (r *DisabledBlockRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *DisabledBlockRule) Category() string                 { return "Style" }

func (r *DisabledBlockRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	// The DISABLED_ RawType prefix is the primary signal (Power Automate marks
	// disabled actions this way). The "(disabled)" name substring is a secondary
	// heuristic for renamed actions — but it must NOT match COMMENT blocks,
	// whose Name carries arbitrary comment text (e.g. "# TODO re-enable
	// (disabled) action"), which would otherwise be a false positive.
	isDisabled := strings.HasPrefix(block.RawType, "DISABLED_") ||
		(block.Type != models.BlockTypeComment && strings.Contains(block.Name, "(disabled)"))
	if !isDisabled {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Disabled block",
		Description: "Block '" + block.Name + "' is disabled.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Remove disabled blocks to keep the flow clean, or re-enable them if they are needed.",
	}}
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&DisabledBlockRule{}) }
