package analyzer

import (
	"pad-core/models"
)

type SwitchNoDefaultRule struct{}

func (r *SwitchNoDefaultRule) ID() string   { return "switch-no-default" }
func (r *SwitchNoDefaultRule) Name() string { return "Switch without default case" }
func (r *SwitchNoDefaultRule) Description() string {
	return "SWITCH blocks that lack a DEFAULT case, leaving unhandled enum values to silently fall through."
}
func (r *SwitchNoDefaultRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *SwitchNoDefaultRule) Category() string                 { return "Logic" }

func (r *SwitchNoDefaultRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeSwitch {
		return nil
	}
	for ci := range block.Children {
		if block.Children[ci].Type == models.BlockTypeDefault {
			return nil
		}
	}
	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Switch without default case",
		Description: "This SWITCH block has no DEFAULT case, so unexpected values are silently ignored.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Add a DEFAULT case to handle unexpected values or log a warning.",
	}}
}

func init() { registerRule(&SwitchNoDefaultRule{}) }
