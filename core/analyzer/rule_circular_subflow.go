package analyzer

import (
	"pad-core/models"
)

type CircularSubflowRule struct{}

func (r *CircularSubflowRule) ID() string   { return "circular-subflow-dependency" }
func (r *CircularSubflowRule) Name() string { return "Circular subflow dependency" }
func (r *CircularSubflowRule) Description() string {
	return "Subflows that participate in a circular call chain, causing infinite recursion at runtime."
}
func (r *CircularSubflowRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *CircularSubflowRule) Category() string                 { return "Logic" }

func (r *CircularSubflowRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if len(ctx.CircularSubflows) == 0 {
		return nil
	}

	subflow := ctx.SubflowByID[block.SubflowID]
	if subflow == nil {
		return nil
	}
	if !ctx.CircularSubflows[subflow.ID] {
		return nil
	}

	// Only emit once per subflow — on the first block of the subflow (the
	// subflow header / first action) to avoid duplicate findings for every
	// block inside the same cyclic subflow.
	if len(subflow.Blocks) == 0 || block.ID != subflow.Blocks[0].ID {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Circular subflow dependency",
		Description: "Subflow '" + subflow.Name + "' is part of a circular call chain. This will cause infinite recursion at runtime.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Break the cycle by removing one call in the chain or by introducing a termination condition.",
		Metadata:    map[string]interface{}{"subflow": subflow.Name},
	}}
}

func init() { registerRule(&CircularSubflowRule{}) }
