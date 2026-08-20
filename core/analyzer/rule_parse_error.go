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

	findings := make([]models.Finding, 0, len(ctx.Flow.ParseErrors))
	for _, pe := range ctx.Flow.ParseErrors {
		sev := models.SeverityError
		switch pe.Severity {
		case "warning":
			sev = models.SeverityWarning
		case "info":
			sev = models.SeverityInfo
		}
		findings = append(findings, models.Finding{
			RuleID:      r.ID(),
			Severity:    sev,
			Title:       "Parse error (line " + itoa(pe.Line) + ")",
			Description: pe.Message,
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Fix the syntax error so the parser can produce a complete block tree.",
			Metadata:    map[string]interface{}{"line": pe.Line, "column": pe.Column, "snippet": pe.Snippet},
		})
	}

	return findings
}

func init() { registerRule(&ParseErrorRule{}) }
