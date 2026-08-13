package analyzer

import (
	"strings"

	"pad-core/models"
)

// TaintedSinkRule flags dangerous sinks (shell commands, SQL, HTTP, file
// writes, process launches) that directly reference user-controlled input
// variables (Input_*/input_*/UserInput_* prefix). This is the classic taint-
// tracking finding: untrusted data reaches a dangerous sink without validation.
//
// The full data-flow engine (computeTaintPaths) tracks indirect flows through
// intermediate assignments; this rule catches the common direct case — the
// source variable is used verbatim in the sink block's properties — as a
// per-block O(1) check during the main analysis walk.
type TaintedSinkRule struct{}

func (r *TaintedSinkRule) ID() string   { return "tainted-sink" }
func (r *TaintedSinkRule) Name() string { return "Tainted data in dangerous sink" }
func (r *TaintedSinkRule) Description() string {
	return "User-controlled input (Input_/UserInput_ variables) flows directly into a dangerous sink (command execution, SQL, HTTP, file write). Validate/sanitize before use."
}
func (r *TaintedSinkRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *TaintedSinkRule) Category() string                 { return "Security" }

func (r *TaintedSinkRule) Confidence() models.Confidence { return models.ConfidenceMedium }

// taintedInputPrefixes — variable name prefixes that mark user-controlled data.
var taintedInputPrefixes = []string{
	"Input_",
	"input_",
	"UserInput_",
	"userinput_",
}

func isTaintedSourceVar(varName string) bool {
	for _, p := range taintedInputPrefixes {
		if strings.HasPrefix(varName, p) {
			return true
		}
	}
	return false
}

func (r *TaintedSinkRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	sinkType := findSink(block.RawType)
	if sinkType == "" {
		return nil
	}
	for _, v := range block.Variables {
		if isTaintedSourceVar(v) {
			return []models.Finding{{
				RuleID:      r.ID(),
				Severity:    r.DefaultSeverity(),
				Title:       "Tainted data in dangerous sink",
				Description: "User-controlled variable %" + v + " flows directly into a " + sinkType + " sink without validation. A malicious value can inject commands, alter queries, or exfiltrate data.",
				BlockID:     block.ID,
				SubflowID:   block.SubflowID,
				Suggestion:  "Validate/allow-list the input before passing it to this action, or use a parameterized API that separates code from data.",
				Metadata:    map[string]interface{}{"variable": v, "sinkType": sinkType},
			}}
		}
	}
	return nil
}

func init() { registerRule(&TaintedSinkRule{}) }
