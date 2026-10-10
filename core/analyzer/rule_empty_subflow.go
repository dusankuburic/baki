package analyzer

import (
	"pad-core/models"
)

type EmptySubflowRule struct{}

func (r *EmptySubflowRule) ID() string   { return "empty-subflow" }
func (r *EmptySubflowRule) Name() string { return "Empty subflow" }
func (r *EmptySubflowRule) Description() string {
	return "Subflows that contain no actionable blocks — dead code that adds clutter."
}
func (r *EmptySubflowRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *EmptySubflowRule) Category() string                 { return "Style" }

func (r *EmptySubflowRule) CheckFlow(ctx *RuleContext) []models.Finding {
	var out []models.Finding
	for i := range ctx.Flow.Subflows {
		sf := &ctx.Flow.Subflows[i]
		// Fire on the first top-level block (any type) of the subflow; a
		// subflow without blocks has no anchor and is not flagged.
		anchor := subflowAnchor(ctx, sf)
		if anchor == nil || countActionableBlocks(sf.Blocks) > 0 {
			continue
		}
		out = append(out, models.Finding{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Empty subflow",
			Description: "Subflow '" + sf.Name + "' contains no actionable blocks.",
			BlockID:     anchor.ID,
			SubflowID:   sf.ID,
			Suggestion:  "Remove the empty subflow or add actions to it.",
			Metadata:    map[string]interface{}{"subflow": sf.Name},
		})
	}
	return out
}

// countActionableBlocks counts non-comment, non-END blocks in the tree.
func countActionableBlocks(blocks []models.Block) int {
	count := 0
	var walk func([]models.Block)
	walk = func(bs []models.Block) {
		for i := range bs {
			b := &bs[i]
			if b.Type != models.BlockTypeComment && b.Type != models.BlockTypeEnd {
				count++
			}
			walk(b.Children)
		}
	}
	walk(blocks)
	return count
}

// No AutoFix: remove-block would strip one action per iteration, and
// subflows may be invoked dynamically.
func init() {
	registerRule(&EmptySubflowRule{}, RuleMeta{
		Confidence:   models.ConfidenceMedium,
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
