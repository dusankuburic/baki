package analyzer

import (
	"fmt"

	"pad-analyzer/internal/models"
)

// SubflowNoErrorHandlerRule flags non-trivial subflows that have no error-handler
// block anywhere in their block tree. A subflow is considered non-trivial when it
// contains more than 3 blocks that are not comments, variable declarations, or
// structural terminators.
type SubflowNoErrorHandlerRule struct{}

func (r *SubflowNoErrorHandlerRule) ID() string              { return "subflow-no-error-handler" }
func (r *SubflowNoErrorHandlerRule) Name() string             { return "Subflow without error handler" }
func (r *SubflowNoErrorHandlerRule) Description() string      { return "Non-trivial subflows that contain no error handler block." }
func (r *SubflowNoErrorHandlerRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *SubflowNoErrorHandlerRule) Category() string         { return "Reliability" }

func (r *SubflowNoErrorHandlerRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	// Fire once per subflow by triggering only on the first top-level block
	// (no parent entry in ParentMap, BlockIndex == 0).
	if _, hasParent := ctx.ParentMap[block.ID]; hasParent {
		return nil
	}
	if ctx.BlockIndex[block.ID] != 0 {
		return nil
	}

	// Locate the subflow this block belongs to.
	var sf *models.Subflow
	for i := range ctx.Flow.Subflows {
		if ctx.Flow.Subflows[i].ID == block.SubflowID {
			sf = &ctx.Flow.Subflows[i]
			break
		}
	}
	if sf == nil {
		return nil
	}

	if !sfNeedsErrorHandler(sf.Blocks) {
		return nil
	}

	// Check whether any error-handler block exists anywhere in this subflow.
	for _, eh := range ctx.BlocksByType[models.BlockTypeErrorHandler] {
		if eh.SubflowID == sf.ID {
			return nil
		}
	}

	return []models.Finding{{
		RuleID:    r.ID(),
		Severity:  r.DefaultSeverity(),
		Title:     "Subflow without error handler",
		Description: fmt.Sprintf("Subflow %q has action blocks but no error handler. Unhandled errors will silently terminate the subflow.", sf.Name),
		SubflowID: sf.ID,
		Suggestion: "Add a Try/Catch or On Block Error handler to protect against unexpected failures.",
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
