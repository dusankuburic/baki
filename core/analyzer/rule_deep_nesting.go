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

var deepNestingMaxDepth = OptionSpec{Key: "maxDepth", Default: 6, Min: 1}

func (r *DeepNestingRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	maxDepth := ruleOption(ctx.Settings, r.ID(), deepNestingMaxDepth)

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

func init() {
	registerRule(&DeepNestingRule{}, RuleMeta{
		Confidence:   models.ConfidenceLow,
		Options:      []OptionSpec{deepNestingMaxDepth},
		Related:      []Related{{To: "infinite-loop-risk", Reason: "deep nesting increases loop complexity"}},
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
