package analyzer

import (
	"strings"

	"pad-analyzer/internal/models"
)

type InfiniteLoopRiskRule struct{}

func (r *InfiniteLoopRiskRule) ID() string          { return "infinite-loop-risk" }
func (r *InfiniteLoopRiskRule) Name() string         { return "Loop may run forever" }
func (r *InfiniteLoopRiskRule) Description() string  { return "LOOP blocks with no recognizable exit condition." }
func (r *InfiniteLoopRiskRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *InfiniteLoopRiskRule) Category() string     { return "Reliability" }

func (r *InfiniteLoopRiskRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeLoop {
		return nil
	}

	if hasExitCondition(block) {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Loop may run forever",
		Description: "This loop has no recognizable exit condition such as Exit Loop, Break, or modifications to the loop variable.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Add an 'Exit loop' action or ensure the loop variable is modified to guarantee termination.",
	}}
}

func hasExitCondition(loop *models.Block) bool {
	found := false
	walkBlockTree(loop, func(b *models.Block) {
		if b.ID == loop.ID {
			return
		}
		if isExitLoop(b) {
			found = true
			return
		}
		for _, v := range b.Properties {
			if strings.Contains(v, "Exit") || strings.Contains(v, "Break") {
				found = true
				return
			}
		}
	})
	return found
}
