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

func (r *EmptySubflowRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if _, hasParent := ctx.ParentMap[block.ID]; hasParent {
		return nil
	}
	subflow := ctx.SubflowByID[block.SubflowID]
	if subflow == nil {
		return nil
	}
	// Fire on the first top-level block (any type) of the subflow.
	if len(subflow.Blocks) == 0 || block.ID != subflow.Blocks[0].ID {
		return nil
	}
	if countActionableBlocks(subflow.Blocks) > 0 {
		return nil
	}
	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Empty subflow",
		Description: "Subflow '" + subflow.Name + "' contains no actionable blocks.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Remove the empty subflow or add actions to it.",
		Metadata:    map[string]interface{}{"subflow": subflow.Name},
	}}
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

func init() { registerRule(&EmptySubflowRule{}) }
