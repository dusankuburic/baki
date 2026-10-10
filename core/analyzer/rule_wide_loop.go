package analyzer

import (
	"pad-core/models"
)

type WideLoopRule struct{}

func (r *WideLoopRule) ID() string   { return "wide-loop" }
func (r *WideLoopRule) Name() string { return "Loop body is too large" }
func (r *WideLoopRule) Description() string {
	return "Loops containing more than N action blocks, indicating logic that should be extracted to subflows."
}
func (r *WideLoopRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *WideLoopRule) Category() string                 { return "Style" }

var wideLoopMaxBlocks = OptionSpec{Key: "maxBlocks", Default: 20, Min: 1}

func (r *WideLoopRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeLoop {
		return nil
	}

	maxBlocks := ruleOption(ctx.Settings, r.ID(), wideLoopMaxBlocks)

	// O(1) subtree size from the precomputed DescendantCount index (the old
	// per-LOOP subtree walk re-visited every nested loop's body once per
	// ancestor, O(n·depth) on deep loop chains).
	count := ctx.DescendantCount[block.ID]

	if count <= maxBlocks {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Loop body is too large",
		Description: "This loop contains blocks that should be extracted into subflows for better readability and maintainability.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Extract part of the loop body into a separate subflow. Aim for loops with fewer action blocks inside.",
		Metadata:    map[string]interface{}{"blockCount": count, "maxBlocks": maxBlocks},
	}}
}

func init() {
	registerRule(&WideLoopRule{}, RuleMeta{
		Confidence:   models.ConfidenceLow,
		Options:      []OptionSpec{wideLoopMaxBlocks},
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
