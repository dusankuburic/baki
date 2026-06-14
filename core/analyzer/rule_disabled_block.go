package analyzer

import (
	"strings"

	"pad-core/models"
)

type DisabledBlockRule struct{}

func (r *DisabledBlockRule) ID() string          { return "disabled-block" }
func (r *DisabledBlockRule) Name() string         { return "Disabled block" }
func (r *DisabledBlockRule) Description() string  { return "Disabled blocks left in the flow should be removed or re-enabled before production." }
func (r *DisabledBlockRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *DisabledBlockRule) Category() string     { return "Style" }

func (r *DisabledBlockRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if !strings.HasPrefix(block.RawType, "DISABLED_") &&
		!strings.Contains(block.Name, "(disabled)") {
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
