package analyzer

import (
	"pad-core/models"
)

type EmptyBranchRule struct{}

func (r *EmptyBranchRule) ID() string   { return "empty-branch" }
func (r *EmptyBranchRule) Name() string { return "Empty condition branch" }
func (r *EmptyBranchRule) Description() string {
	return "IF/SWITCH/CASE branches with no action blocks inside."
}
func (r *EmptyBranchRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *EmptyBranchRule) Category() string                 { return "Style" }

func (r *EmptyBranchRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	switch block.Type {
	case models.BlockTypeCondition, models.BlockTypeSwitch:
	default:
		return nil
	}

	var findings []models.Finding

	for i := range block.Children {
		child := &block.Children[i]
		switch child.Type {
		case models.BlockTypeElse, models.BlockTypeCase, models.BlockTypeDefault:
		default:
			continue
		}

		if hasActionChildren(child) {
			continue
		}

		findings = append(findings, models.Finding{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Empty condition branch",
			Description: "The " + branchLabel(child.Type) + " branch of this " + blockTypeLabel(block.Type) + " has no actions inside it.",
			BlockID:     child.ID,
			SubflowID:   child.SubflowID,
			Suggestion:  "Add actions to this branch, remove it entirely, or add a comment explaining why it's intentionally empty.",
		})
	}

	return findings
}

func hasActionChildren(block *models.Block) bool {
	for i := range block.Children {
		child := &block.Children[i]
		if child.Type == models.BlockTypeEnd {
			continue
		}
		if child.Type == models.BlockTypeComment {
			continue
		}
		return true
	}
	return false
}

func branchLabel(t models.BlockType) string {
	switch t {
	case models.BlockTypeElse:
		return "Else"
	case models.BlockTypeCase:
		return "Case"
	case models.BlockTypeDefault:
		return "Default"
	default:
		return string(t)
	}
}

func blockTypeLabel(t models.BlockType) string {
	switch t {
	case models.BlockTypeCondition:
		return "If"
	case models.BlockTypeSwitch:
		return "Switch"
	default:
		return string(t)
	}
}
