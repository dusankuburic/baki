package analyzer

import (
	"pad-core/models"
)

type WaitZeroRule struct{}

func (r *WaitZeroRule) ID() string   { return "wait-zero" }
func (r *WaitZeroRule) Name() string { return "Wait zero" }
func (r *WaitZeroRule) Description() string {
	return "WAIT 0 actions that are effectively no-ops and add unnecessary clutter."
}
func (r *WaitZeroRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *WaitZeroRule) Category() string                 { return "Style" }

func (r *WaitZeroRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeWait && block.RawType != "WAIT" {
		return nil
	}
	// The tokenizer parses WAIT <n> into SetVarName/SetVarValue for SET, but
	// WAIT actions carry the duration in the Content/Raw field. Check the raw
	// text for a zero value.
	raw := block.Name
	if raw == "" {
		raw = block.RawType
	}
	if !isWaitZero(raw) {
		return nil
	}
	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Wait zero",
		Description: "This WAIT action has a duration of 0, making it a no-op.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Remove the WAIT 0 action or increase the duration.",
	}}
}

// isWaitZero checks if the raw text of a WAIT action specifies a zero duration.
// Matches "WAIT 0", "WAIT 0.0", "WAIT (0)", etc.
func isWaitZero(raw string) bool {
	s := raw
	if len(s) < 6 {
		return false
	}
	// Extract the numeric part after "WAIT "
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			rest := s[i:]
			// Check if all remaining non-space chars are '0', '.', or ')'
			for _, c := range rest {
				if c == ' ' || c == '\t' {
					break
				}
				if c != '0' && c != '.' && c != ')' && c != '(' {
					return false
				}
			}
			return true
		}
	}
	return false
}

func init() { registerRule(&WaitZeroRule{}) }
