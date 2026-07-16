package analyzer

import (
	"pad-core/models"
	"regexp"
)

type TodoInCommentRule struct{}

func (r *TodoInCommentRule) ID() string   { return "todo-in-comment" }
func (r *TodoInCommentRule) Name() string { return "TODO in comment" }
func (r *TodoInCommentRule) Description() string {
	return "COMMENT blocks containing TODO, FIXME, XXX, or HACK markers that signal unfinished work."
}
func (r *TodoInCommentRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *TodoInCommentRule) Category() string                 { return "Style" }

var todoPattern = regexp.MustCompile(`(?i)\b(TODO|FIXME|XXX|HACK)\b`)

func (r *TodoInCommentRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeComment {
		return nil
	}
	text := block.Name
	if text == "" {
		return nil
	}
	loc := todoPattern.FindStringIndex(text)
	if loc == nil {
		return nil
	}
	marker := text[loc[0]:loc[1]]
	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       marker + " in comment",
		Description: "This comment contains a " + marker + " marker: \"" + truncate(text, 80) + "\"",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Resolve the TODO or convert it to a tracked issue.",
	}}
}

func init() { registerRule(&TodoInCommentRule{}) }
