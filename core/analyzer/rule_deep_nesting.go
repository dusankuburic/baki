package analyzer

import (
	"pad-core/models"
)

type DeepNestingRule struct{}

func (r *DeepNestingRule) ID() string                       { return "deep-nesting" }
func (r *DeepNestingRule) Name() string                     { return "Deeply nested logic" }
func (r *DeepNestingRule) Description() string              { return "Blocks nested more than 6 levels deep." }
func (r *DeepNestingRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *DeepNestingRule) Category() string                 { return "Style" }

func (r *DeepNestingRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	maxDepth := 6
	if ctx.Settings != nil {
		if rc, ok := ctx.Settings.Analysis.Rules[r.ID()]; ok {
			if md, ok := rc.Options["maxDepth"]; ok {
				if f, ok := md.(float64); ok && f > 0 {
					maxDepth = int(f)
				}
			}
		}
	}

	depth := ctx.BlockDepth[block.ID]
	if depth <= maxDepth {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Deeply nested logic",
		Description: "This block is nested beyond the recommended depth, which reduces readability.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Consider extracting nested logic into a subflow to improve readability and maintainability.",
		Metadata:    map[string]interface{}{"depth": depth, "maxDepth": maxDepth},
	}}
}
