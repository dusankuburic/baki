package analyzer

import (
	"strings"

	"pad-core/models"
)

type ErrorSwallowRule struct{}

func (r *ErrorSwallowRule) ID() string                    { return "error-swallow" }
func (r *ErrorSwallowRule) Name() string                   { return "Error handler swallows errors" }
func (r *ErrorSwallowRule) Description() string            { return "Error handlers that catch errors but don't log, re-raise, or set any error variable." }
func (r *ErrorSwallowRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *ErrorSwallowRule) Category() string               { return "Reliability" }

func (r *ErrorSwallowRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeErrorHandler {
		return nil
	}

	if len(block.Children) == 0 {
		return nil
	}

	if handlerDoesSomething(block) {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Error handler swallows errors silently",
		Description: "This error handler catches errors but doesn't appear to log them, set error variables, or take any meaningful action.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Add logging, set an error variable, re-raise the error, or notify the user. Silent error swallowing hides problems.",
		AutoFixHint: "Inside the On Error block, add: Variables.Set Variable > Name: ErrorMessage, Value: %LastError%  — then add a Message Box or Write to File action to surface the error.",
	}}
}

func handlerDoesSomething(handler *models.Block) bool {
	found := false
	walkBlockTree(handler, func(b *models.Block) {
		if b.ID == handler.ID {
			return
		}
		if b.Type == models.BlockTypeEnd || b.Type == models.BlockTypeComment {
			return
		}

		if b.Type == models.BlockTypeVariable {
			if b.Properties != nil {
				for k := range b.Properties {
					kl := strings.ToLower(k)
					if strings.Contains(kl, "output") || strings.Contains(kl, "var") {
						found = true
						return
					}
				}
			}
		}

		for _, v := range b.Variables {
			vl := strings.ToLower(v)
			if strings.Contains(vl, "error") || strings.Contains(vl, "exception") || strings.Contains(vl, "lasterror") {
				found = true
				return
			}
		}

		if b.Properties != nil {
			for _, v := range b.Properties {
				vl := strings.ToLower(v)
				if strings.Contains(vl, "error") || strings.Contains(vl, "exception") || strings.Contains(vl, "log") {
					found = true
					return
				}
			}
		}

		rt := strings.ToLower(b.RawType)
		if strings.Contains(rt, "log") || strings.Contains(rt, "write") ||
			strings.Contains(rt, "send") || strings.Contains(rt, "message") ||
			strings.Contains(rt, "exit") || strings.Contains(rt, "terminate") ||
			strings.Contains(rt, "display") || strings.Contains(rt, "show") {
			found = true
			return
		}

		if b.Type == models.BlockTypeAction {
			found = true
			return
		}
	})
	return found
}
