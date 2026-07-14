package analyzer

import (
	"strings"

	"pad-core/models"
)

type RedundantActionRule struct{}

func (r *RedundantActionRule) ID() string   { return "redundant-action" }
func (r *RedundantActionRule) Name() string { return "Redundant action detected" }
func (r *RedundantActionRule) Description() string {
	return "Actions that have no effect, such as setting a variable to itself or consecutive duplicate conversions."
}
func (r *RedundantActionRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *RedundantActionRule) Category() string                 { return "Performance" }

func (r *RedundantActionRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	findings := r.checkSelfAssignment(block, ctx)
	findings = append(findings, r.checkConsecutiveConversion(block, ctx)...)
	return findings
}

func (r *RedundantActionRule) checkSelfAssignment(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeVariable {
		return nil
	}

	outVar := outputVar(block)
	if outVar == "" {
		return nil
	}

	value := block.Properties["_value"]
	if value == "" {
		return nil
	}

	if value == "%"+outVar+"%" {
		return []models.Finding{{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Variable set to itself",
			Description: "Variable '" + outVar + "' is assigned its own value (%" + outVar + "%), which has no effect.",
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Remove this action or change the value being assigned.",
			Metadata:    map[string]any{"variable": outVar, "pattern": "self-assignment"},
		}}
	}

	return nil
}

func (r *RedundantActionRule) checkConsecutiveConversion(block *models.Block, ctx *RuleContext) []models.Finding {
	if !strings.HasPrefix(block.RawType, "Variables.") {
		return nil
	}

	if !isConversionAction(block.RawType) {
		return nil
	}

	siblings := GetSiblings(ctx, block)
	if len(siblings) < 2 {
		return nil
	}

	// Use the precomputed sibling index instead of an O(siblings) self-scan.
	myIdx, ok := ctx.BlockIndex[block.ID]
	if !ok || myIdx <= 0 {
		return nil
	}

	prev := siblings[myIdx-1]
	if prev.RawType != block.RawType {
		return nil
	}

	outVar := outputVar(block)
	prevOutput := outputVar(prev)

	prevValue := prev.Properties["_value"]
	if prevValue == "" {
		prevValue = prev.Properties["_input"]
	}

	if prevOutput != "" && outVar != "" && prevValue == "%"+outVar+"%" {
		return []models.Finding{{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Redundant consecutive conversion",
			Description: "This conversion is the same type as the immediately preceding one and operates on its output, suggesting a no-op chain.",
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Remove the redundant conversion or verify the data type is correct.",
			Metadata:    map[string]any{"pattern": "consecutive-conversion", "rawType": block.RawType},
		}}
	}

	return nil
}

func isConversionAction(rawType string) bool {
	conversionTypes := []string{
		"Variables.ConvertTextToNumber",
		"Variables.ConvertNumberToText",
		"Variables.GetTextLength",
		"Variables.TrimText",
		"Variables.ToUpper",
		"Variables.ToLower",
	}
	for _, ct := range conversionTypes {
		if rawType == ct {
			return true
		}
	}
	return false
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&RedundantActionRule{}) }
