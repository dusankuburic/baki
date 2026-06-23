package analyzer

import (
	"strings"

	"pad-core/models"
)

type SlowPatternRule struct{}

func (r *SlowPatternRule) ID() string   { return "slow-pattern" }
func (r *SlowPatternRule) Name() string { return "UI automation in tight loop" }
func (r *SlowPatternRule) Description() string {
	return "UI automation actions inside a loop without a delay action."
}
func (r *SlowPatternRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *SlowPatternRule) Category() string                 { return "Performance" }

func (r *SlowPatternRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeLoop {
		return nil
	}

	hasUI := false
	hasWait := false

	walkBlockTree(block, func(b *models.Block) {
		if b.ID == block.ID {
			return
		}
		if strings.HasPrefix(b.RawType, "WebAutomation.") || strings.HasPrefix(b.RawType, "UIAutomation.") {
			hasUI = true
		}
		if strings.Contains(b.RawType, "Wait") || strings.Contains(b.RawType, "Delay") {
			hasWait = true
		}
	})

	if !hasUI || hasWait {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "UI automation in tight loop",
		Description: "This loop contains UI/web automation actions without any delay, which can overwhelm the target application.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Add a 'Wait' or 'Delay' action inside the loop to reduce load on the target application.",
	}}
}
