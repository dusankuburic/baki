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

func (r *WideLoopRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeLoop {
		return nil
	}

	maxBlocks := 20
	if ctx.Settings != nil {
		if rc, ok := ctx.Settings.Analysis.Rules[r.ID()]; ok {
			if mb, ok := rc.Options["maxBlocks"]; ok {
				if f, ok := mb.(float64); ok && f > 0 {
					maxBlocks = int(f)
				}
			}
		}
	}

	count := 0
	walkBlockTree(block, func(b *models.Block) {
		if b.ID == block.ID {
			return
		}
		if b.Type == models.BlockTypeEnd || b.Type == models.BlockTypeComment {
			return
		}
		count++
	})

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

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&WideLoopRule{}) }
