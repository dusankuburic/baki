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

	// Own-body semantics (walkLoopBody skips nested-loop subtrees): a WAIT
	// inside a nested loop only paces that inner loop's iterations, not this
	// loop's — and the nested loop's UI actions are reported on the nested
	// loop itself when it lacks a wait. Previously each ancestor loop re-walk
	// its full subtree, making deeply nested chains O(n·depth).
	walkLoopBody(block, func(b *models.Block) {
		if strings.HasPrefix(b.RawType, "WebAutomation.") || strings.HasPrefix(b.RawType, "UIAutomation.") {
			hasUI = true
		}
		rtLower := strings.ToLower(b.RawType)
		if strings.Contains(rtLower, "wait") || strings.Contains(rtLower, "delay") {
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

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&SlowPatternRule{}) }
