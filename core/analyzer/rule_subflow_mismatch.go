package analyzer

import (
	"strings"

	"pad-core/models"
)

type SubflowMismatchRule struct{}

func (r *SubflowMismatchRule) ID() string   { return "subflow-mismatch" }
func (r *SubflowMismatchRule) Name() string { return "Subflow call parameter mismatch" }
func (r *SubflowMismatchRule) Description() string {
	return "Subflow calls where output variables aren't captured or input variables may be missing."
}
func (r *SubflowMismatchRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *SubflowMismatchRule) Category() string                 { return "Logic" }

func (r *SubflowMismatchRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeSubflow && block.RawType != "CALL" {
		return nil
	}

	targetName := resolveSubflowTarget(block)
	if targetName == "" {
		return nil
	}

	target := ctx.SubflowByName[targetName]
	if target == nil {
		return nil
	}

	var findings []models.Finding

	if hasOutputVariables(target) && !capturesOutput(block) {
		findings = append(findings, models.Finding{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Subflow output not captured",
			Description: "Subflow '" + targetName + "' produces output variables but this call does not capture them.",
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Store the subflow's output in a variable so the result is not lost.",
			AutoFix:     "append-output",
			Metadata:    map[string]interface{}{"targetSubflow": targetName, "pattern": "uncaptured-output"},
		})
	}

	if hasInputVariables(target) && !providesInputs(block, target) {
		findings = append(findings, models.Finding{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Subflow input may not be provided",
			Description: "Subflow '" + targetName + "' expects input variables but this call may not provide all of them.",
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Ensure all required input variables are passed to this subflow call.",
			Metadata:    map[string]interface{}{"targetSubflow": targetName, "pattern": "missing-inputs"},
		})
	}

	return findings
}

func resolveSubflowTarget(block *models.Block) string {
	if strings.HasPrefix(block.Name, "Call ") {
		name := strings.TrimPrefix(block.Name, "Call ")
		name = strings.TrimSuffix(name, " (disabled)")
		return strings.TrimSpace(name)
	}
	for _, t := range block.Tokens {
		if t.Type == "subflow" && t.Target != "" {
			return t.Target
		}
	}
	if block.Properties != nil {
		if t, ok := block.Properties["subflowName"]; ok && t != "" {
			return t
		}
		if t, ok := block.Properties["_target"]; ok && t != "" {
			return t
		}
	}
	return ""
}

func hasOutputVariables(sf *models.Subflow) bool {
	for _, v := range sf.Variables {
		if strings.HasPrefix(v.Name, "Output_") || strings.HasPrefix(v.Name, "output_") {
			return true
		}
		if v.Scope == "output" {
			return true
		}
	}
	return false
}

func hasInputVariables(sf *models.Subflow) bool {
	for _, v := range sf.Variables {
		if strings.HasPrefix(v.Name, "Input_") || strings.HasPrefix(v.Name, "input_") {
			return true
		}
		if v.Scope == "input" {
			return true
		}
	}
	return false
}

func capturesOutput(block *models.Block) bool {
	if block.Properties == nil {
		return false
	}
	_, hasOutput := block.Properties["_output"]
	if !hasOutput {
		_, hasOutput = block.Properties["outputVariable"]
	}
	return hasOutput
}

func providesInputs(block *models.Block, target *models.Subflow) bool {
	blockVars := make(map[string]bool)
	for _, v := range block.Variables {
		blockVars[strings.ToLower(v)] = true
	}
	if block.Properties != nil {
		for _, v := range block.Properties {
			v = strings.TrimSpace(v)
			if len(v) >= 2 && strings.HasPrefix(v, "%") && strings.HasSuffix(v, "%") {
				blockVars[strings.ToLower(v[1:len(v)-1])] = true
			}
		}
	}

	for _, iv := range target.Variables {
		if iv.Scope != "input" && !strings.HasPrefix(iv.Name, "Input_") && !strings.HasPrefix(iv.Name, "input_") {
			continue
		}
		if !blockVars[strings.ToLower(iv.Name)] {
			return false
		}
	}
	return true
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&SubflowMismatchRule{}) }
