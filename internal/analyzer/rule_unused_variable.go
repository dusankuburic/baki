package analyzer

import (
	"pad-analyzer/internal/models"
)

type UnusedVariableRule struct{}

func (r *UnusedVariableRule) ID() string          { return "unused-variable" }
func (r *UnusedVariableRule) Name() string         { return "Variable declared but never used" }
func (r *UnusedVariableRule) Description() string  { return "Variables that are declared but never referenced." }
func (r *UnusedVariableRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *UnusedVariableRule) Category() string     { return "Style" }

func (r *UnusedVariableRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeVariable {
		return nil
	}

	// block.Variables contains *referenced* variables from %...% expressions.
	// The declared (output) variable is stored in Properties["_output"] or "_var".
	declaredVar := outputVar(block)
	if declaredVar == "" {
		return nil
	}

	if ctx.UsedVariables[declaredVar] {
		return nil
	}

	return []models.Finding{{
		RuleID:     r.ID(),
		Severity:   r.DefaultSeverity(),
		Title:      "Variable declared but never used",
		Description: "This variable is set but never referenced elsewhere in the flow.",
		BlockID:    block.ID,
		SubflowID:  block.SubflowID,
		Suggestion: "Remove the unused variable or add a reference to it.",
		Metadata:   map[string]any{"variable": declaredVar},
	}}
}
