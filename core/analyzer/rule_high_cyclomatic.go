package analyzer

import (
	"pad-core/models"
)

type HighCyclomaticComplexityRule struct{}

func (r *HighCyclomaticComplexityRule) ID() string   { return "high-cyclomatic-complexity" }
func (r *HighCyclomaticComplexityRule) Name() string { return "High cyclomatic complexity" }
func (r *HighCyclomaticComplexityRule) Description() string {
	return "Subflow whose cyclomatic complexity exceeds the threshold (default 20), indicating excessive branching logic that is hard to test and maintain."
}
func (r *HighCyclomaticComplexityRule) DefaultSeverity() models.Severity {
	return models.SeverityWarning
}
func (r *HighCyclomaticComplexityRule) Category() string { return "Style" }

const defaultMaxCyclomatic = 20

func (r *HighCyclomaticComplexityRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if ctx.SubflowCyclo == nil {
		return nil
	}
	cyclo := ctx.SubflowCyclo[block.SubflowID]
	if cyclo <= defaultMaxCyclomatic {
		return nil
	}

	subflow := ctx.SubflowByID[block.SubflowID]
	if subflow == nil {
		return nil
	}
	// Only emit once per subflow — on the first block.
	if len(subflow.Blocks) == 0 || block.ID != subflow.Blocks[0].ID {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "High cyclomatic complexity",
		Description: "Subflow '" + subflow.Name + "' has a cyclomatic complexity of " + itoa(cyclo) + " (threshold: " + itoa(defaultMaxCyclomatic) + "), indicating excessive branching that is hard to test and maintain.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Split this subflow into smaller subflows, reduce nesting, or extract complex conditions into helper subflows.",
		Metadata:    map[string]interface{}{"complexity": cyclo, "threshold": defaultMaxCyclomatic},
	}}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func init() { registerRule(&HighCyclomaticComplexityRule{}) }
