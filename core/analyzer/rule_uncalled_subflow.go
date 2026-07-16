package analyzer

import (
	"pad-core/models"
)

type UncalledSubflowRule struct{}

func (r *UncalledSubflowRule) ID() string   { return "uncalled-subflow" }
func (r *UncalledSubflowRule) Name() string { return "Uncalled subflow" }
func (r *UncalledSubflowRule) Description() string {
	return "Subflow that is never called by any other subflow and is not the entry point — dead code at the flow level."
}
func (r *UncalledSubflowRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *UncalledSubflowRule) Category() string                 { return "Logic" }

func (r *UncalledSubflowRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if ctx.SubflowFanIn == nil {
		return nil
	}
	if ctx.SubflowFanIn[block.SubflowID] > 0 {
		return nil
	}

	subflow := ctx.SubflowByID[block.SubflowID]
	if subflow == nil {
		return nil
	}
	// The entry-point subflow (first in document order) is the default caller
	// target — never flag it even with fan-in 0.
	if isEntryPoint(subflow, ctx) {
		return nil
	}
	// Only emit once per subflow — on the first block.
	if len(subflow.Blocks) == 0 || block.ID != subflow.Blocks[0].ID {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Uncalled subflow",
		Description: "Subflow '" + subflow.Name + "' is never called by any other subflow and is not the entry point. It is dead code at the flow level.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Remove this subflow if it is no longer needed, or add a call to it from the entry point.",
		Metadata:    map[string]interface{}{"subflow": subflow.Name},
	}}
}

// isEntryPoint returns true for the first subflow in document order (typically
// "Main") — the default entry point that is never flagged as uncalled.
func isEntryPoint(sf *models.Subflow, ctx *RuleContext) bool {
	if len(ctx.Flow.Subflows) > 0 && ctx.Flow.Subflows[0].ID == sf.ID {
		return true
	}
	// Also treat "Main" / "main" as entry point by convention.
	return sf.Name == "Main" || sf.Name == "main"
}

func init() { registerRule(&UncalledSubflowRule{}) }
