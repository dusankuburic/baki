package analyzer

import (
	"fmt"

	"pad-core/models"
)

// SubflowNoErrorHandlerRule flags non-trivial subflows that have no
// error-handler block anywhere in their block tree; non-trivial means more
// than 3 blocks that are not comments, variable declarations, or structural
// terminators.
type SubflowNoErrorHandlerRule struct{}

func (r *SubflowNoErrorHandlerRule) ID() string   { return "subflow-no-error-handler" }
func (r *SubflowNoErrorHandlerRule) Name() string { return "Subflow without error handler" }
func (r *SubflowNoErrorHandlerRule) Description() string {
	return "Non-trivial subflows that contain no error handler block."
}
func (r *SubflowNoErrorHandlerRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *SubflowNoErrorHandlerRule) Category() string                 { return "Reliability" }

func (r *SubflowNoErrorHandlerRule) CheckFlow(ctx *RuleContext) []models.Finding {
	var out []models.Finding
	for i := range ctx.Flow.Subflows {
		sf := &ctx.Flow.Subflows[i]
		// Attach to the first non-comment top-level block so the finding's line /
		// suppression target is an actionable block, not a leading comment. A
		// handler anywhere in the subflow, nested ones included, counts.
		anchor := actionableAnchor(ctx, sf)
		if anchor == nil || !sfNeedsErrorHandler(sf.Blocks) || sfHasErrorHandler(sf.Blocks) {
			continue
		}
		out = append(out, models.Finding{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Subflow without error handler",
			Description: fmt.Sprintf("Subflow %q has action blocks but no error handler. Unhandled errors will silently terminate the subflow.", sf.Name),
			BlockID:     anchor.ID,
			SubflowID:   sf.ID,
			Suggestion:  "Add a Try/Catch or On Block Error handler to protect against unexpected failures.",
		})
	}
	return out
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

func init() {
	registerRule(&SubflowNoErrorHandlerRule{}, RuleMeta{
		Confidence:   models.ConfidenceMedium,
		AutoFix:      "wrap-error-handler",
		Related:      []Related{{To: "unhandled-error", Reason: "subflow error handling relates to general error handling"}},
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
