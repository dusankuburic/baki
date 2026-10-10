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

// CheckFlow anchors every parse error on the document's first block when the
// walk dispatches it (not END); otherwise the engine reports them block-less
// (parseErrorFallback). The parser grades each error's severity.
func (r *ParseErrorRule) CheckFlow(ctx *RuleContext) []models.Finding {
	flow := ctx.Flow
	if len(flow.ParseErrors) == 0 || len(flow.Subflows) == 0 || len(flow.Subflows[0].Blocks) == 0 {
		return nil
	}
	anchor := &flow.Subflows[0].Blocks[0]
	if anchor.Type == models.BlockTypeEnd {
		return nil
	}
	return buildParseErrorFindings(flow.ParseErrors, anchor.ID, anchor.SubflowID)
}

func init() {
	registerRule(&ParseErrorRule{}, RuleMeta{
		Confidence:   models.ConfidenceHigh,
		IntroducedIn: 1,
		// Still 1: a configured severity equal to the default no longer
		// flattens the parser's own grading (severityOverride), but severities
		// move no fingerprint, and a revision would file every new parse error
		// against an older baseline as a new rule's instead of drift.
		Revision: 1,
		Stage:    StageStable,
	})
}
