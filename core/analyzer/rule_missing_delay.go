package analyzer

import (
	"strings"

	"pad-core/models"
)

type MissingDelayRule struct{}

func (r *MissingDelayRule) ID() string   { return "missing-delay" }
func (r *MissingDelayRule) Name() string { return "Web action without wait" }
func (r *MissingDelayRule) Description() string {
	return "Two consecutive WebAutomation/UIAutomation actions with no wait between them."
}
func (r *MissingDelayRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *MissingDelayRule) Category() string                 { return "Reliability" }

func (r *MissingDelayRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if !isWebOrUIAction(block.RawType) {
		return nil
	}
	if isWaitAction(block.RawType) {
		return nil
	}

	siblings := GetSiblings(ctx, block)
	if len(siblings) <= 1 {
		return nil
	}

	// Use the precomputed sibling index instead of an O(siblings) self-scan.
	myIdx, ok := ctx.BlockIndex[block.ID]
	if !ok || myIdx <= 0 {
		return nil
	}

	prev := siblings[myIdx-1]
	if !isWebOrUIAction(prev.RawType) {
		return nil
	}
	if isWaitAction(prev.RawType) {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Web action without wait",
		Description: "Two consecutive web/UI automation actions with no wait between them, which can cause timing failures.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Add a 'Wait for element' or 'Delay' action between UI automation steps to improve reliability.",
	}}
}

func isWebOrUIAction(rawType string) bool {
	return strings.HasPrefix(rawType, "WebAutomation.") || strings.HasPrefix(rawType, "UIAutomation.")
}

func isWaitAction(rawType string) bool {
	return strings.Contains(rawType, "Wait") || strings.Contains(rawType, "Delay")
}
