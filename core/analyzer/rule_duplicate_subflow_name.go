package analyzer

import (
	"pad-core/models"
)

type DuplicateSubflowNameRule struct{}

func (r *DuplicateSubflowNameRule) ID() string   { return "duplicate-subflow-name" }
func (r *DuplicateSubflowNameRule) Name() string { return "Duplicate subflow name" }
func (r *DuplicateSubflowNameRule) Description() string {
	return "Two or more subflows share the same name, causing ambiguous CALL resolution — only the first is used."
}
func (r *DuplicateSubflowNameRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *DuplicateSubflowNameRule) Category() string                 { return "Logic" }

func (r *DuplicateSubflowNameRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if len(ctx.DuplicateSubflowNames) == 0 {
		return nil
	}
	subflow := ctx.SubflowByID[block.SubflowID]
	if subflow == nil {
		return nil
	}
	if !ctx.DuplicateSubflowNames[subflow.Name] {
		return nil
	}
	// Only emit once per subflow — on the first block.
	if len(subflow.Blocks) == 0 || block.ID != subflow.Blocks[0].ID {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Duplicate subflow name",
		Description: "Subflow '" + subflow.Name + "' shares its name with at least one other subflow. Only the first is used when CALL resolves by name — the others are silently unreachable.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Rename one of the duplicate subflows so each has a unique name.",
		Metadata:    map[string]interface{}{"subflow": subflow.Name},
	}}
}

func init() { registerRule(&DuplicateSubflowNameRule{}) }
