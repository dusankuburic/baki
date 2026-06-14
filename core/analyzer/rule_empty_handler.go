package analyzer

import (
	"pad-core/models"
)

type EmptyHandlerRule struct{}

func (r *EmptyHandlerRule) ID() string          { return "empty-handler" }
func (r *EmptyHandlerRule) Name() string         { return "Error handler is empty" }
func (r *EmptyHandlerRule) Description() string  { return "ERROR_HANDLER blocks with no children." }
func (r *EmptyHandlerRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *EmptyHandlerRule) Category() string     { return "Reliability" }

func (r *EmptyHandlerRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeErrorHandler {
		return nil
	}

	hasRealChildren := false
	for _, child := range block.Children {
		if child.Type != models.BlockTypeEnd {
			hasRealChildren = true
			break
		}
	}

	if hasRealChildren {
		return nil
	}

	return []models.Finding{{
		RuleID:     r.ID(),
		Severity:   r.DefaultSeverity(),
		Title:      "Error handler is empty",
		Description: "This error handler has no actions inside it, so errors will be silently ignored.",
		BlockID:    block.ID,
		SubflowID:  block.SubflowID,
		Suggestion: "Add error handling logic such as logging, retrying, or notifying the user.",
	}}
}
