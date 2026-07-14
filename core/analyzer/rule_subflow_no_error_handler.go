package analyzer

import (
	"fmt"

	"pad-core/models"
)

// SubflowNoErrorHandlerRule flags non-trivial subflows that have no error-handler
// block anywhere in their block tree. A subflow is considered non-trivial when it
// contains more than 3 blocks that are not comments, variable declarations, or
// structural terminators.
type SubflowNoErrorHandlerRule struct{}

func (r *SubflowNoErrorHandlerRule) ID() string   { return "subflow-no-error-handler" }
func (r *SubflowNoErrorHandlerRule) Name() string { return "Subflow without error handler" }
func (r *SubflowNoErrorHandlerRule) Description() string {
	return "Non-trivial subflows that contain no error handler block."
}
func (r *SubflowNoErrorHandlerRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *SubflowNoErrorHandlerRule) Category() string                 { return "Reliability" }

func (r *SubflowNoErrorHandlerRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	// Fire once per subflow by triggering only on the first top-level block
	// (no parent entry in ParentMap).
	if _, hasParent := ctx.ParentMap[block.ID]; hasParent {
		return nil
	}
	if block.Type == models.BlockTypeComment {
		return nil
	}

	// Locate the subflow this block belongs to (O(1) via the precomputed index).
	sf := ctx.SubflowByID[block.SubflowID]
	if sf == nil {
		return nil
	}
	// Attach to the first non-comment top-level block so the finding's line /
	// suppression target is an actionable block, not a leading comment.
	if firstActionableBlockID(sf.Blocks) != block.ID {
		return nil
	}

	if !sfNeedsErrorHandler(sf.Blocks) {
		return nil
	}

	// Check whether any error-handler block exists anywhere in this subflow,
	// including handlers nested inside child blocks.
	if sfHasErrorHandler(sf.Blocks) {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Subflow without error handler",
		Description: fmt.Sprintf("Subflow %q has action blocks but no error handler. Unhandled errors will silently terminate the subflow.", sf.Name),
		BlockID:     block.ID,
		SubflowID:   sf.ID,
		Suggestion:  "Add a Try/Catch or On Block Error handler to protect against unexpected failures.",
	}}
}

// sfNeedsErrorHandler returns true when the subflow contains more than 3 blocks
// that are not comments, variable declarations, or end markers.
func sfNeedsErrorHandler(blocks []models.Block) bool {
	count := 0
	var walk func([]models.Block)
	walk = func(bs []models.Block) {
		for i := range bs {
			b := &bs[i]
			switch b.Type {
			case models.BlockTypeComment, models.BlockTypeVariable, models.BlockTypeEnd:
				// exempt from threshold
			default:
				count++
			}
			walk(b.Children)
		}
	}
	walk(blocks)
	return count > 3
}

// sfHasErrorHandler reports whether an error-handler block exists anywhere in
// the block tree, including nested child blocks.
func sfHasErrorHandler(blocks []models.Block) bool {
	for i := range blocks {
		b := &blocks[i]
		if b.Type == models.BlockTypeErrorHandler {
			return true
		}
		if sfHasErrorHandler(b.Children) {
			return true
		}
	}
	return false
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&SubflowNoErrorHandlerRule{}) }
