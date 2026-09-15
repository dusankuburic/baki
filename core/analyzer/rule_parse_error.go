package analyzer

import (
	"pad-core/models"
)

type ParseErrorRule struct{}

func (r *ParseErrorRule) ID() string   { return "parse-error" }
func (r *ParseErrorRule) Name() string { return "Parse error" }
func (r *ParseErrorRule) Description() string {
	return "Syntax errors detected during parsing (unclosed blocks, malformed lines, nesting violations)."
}
func (r *ParseErrorRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *ParseErrorRule) Category() string                 { return "Syntax" }

func (r *ParseErrorRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if len(ctx.Flow.ParseErrors) == 0 {
		return nil
	}
	if len(ctx.Flow.Subflows) == 0 {
		return nil
	}
	firstSF := &ctx.Flow.Subflows[0]
	if len(firstSF.Blocks) == 0 || block.ID != firstSF.Blocks[0].ID {
		return nil
	}

	return buildParseErrorFindings(ctx.Flow.ParseErrors, block.ID, block.SubflowID)
}

func init() { registerRule(&ParseErrorRule{}) }
