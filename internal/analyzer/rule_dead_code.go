package analyzer

import (
	"pad-analyzer/internal/models"
)

type DeadCodeRule struct{}

func (r *DeadCodeRule) ID() string          { return "dead-code" }
func (r *DeadCodeRule) Name() string         { return "Unreachable code" }
func (r *DeadCodeRule) Description() string  { return "Blocks following an unconditional exit/return." }
func (r *DeadCodeRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *DeadCodeRule) Category() string     { return "Style" }

func (r *DeadCodeRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	parentID := ctx.ParentMap[block.ID]
	
	terminatorIdx, hasTerminator := ctx.TerminatorIndex[parentID]
	if !hasTerminator {
		return nil
	}

	myIdx, ok := ctx.BlockIndex[block.ID]
	if !ok {
		return nil
	}

	if myIdx <= terminatorIdx {
		return nil
	}

	return []models.Finding{{
		RuleID:     r.ID(),
		Severity:   r.DefaultSeverity(),
		Title:      "Unreachable code",
		Description: "This block follows an unconditional exit action and will never be executed.",
		BlockID:    block.ID,
		SubflowID:  block.SubflowID,
		Suggestion: "Remove this unreachable code or move it before the exit action.",
	}}
}
