package analyzer

import (
	"strings"
	"pad-core/models"
)

type UninitializedVariableRule struct{}

func (r *UninitializedVariableRule) ID() string          { return "uninitialized-variable" }
func (r *UninitializedVariableRule) Name() string         { return "Variable used before being initialized" }
func (r *UninitializedVariableRule) Description() string  { return "Detects variables that are referenced but never assigned in the flow." }
func (r *UninitializedVariableRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *UninitializedVariableRule) Category() string     { return "Logic" }

func (r *UninitializedVariableRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	findings := []models.Finding{}

	// Variables used in this block (extracted by parser from %...%)
	used := block.Variables
	if len(used) == 0 {
		return nil
	}

	for _, vname := range used {
		// Heuristic: If it's an Input variable, assume it's provided externally.
		if strings.HasPrefix(strings.ToLower(vname), "input_") {
			continue
		}

		// Heuristic: If it's a common system variable (e.g. CurrentDateTime, LoopIndex), skip.
		if isSystemVariable(vname) {
			continue
		}

		if isAssignedAnywhere(vname, ctx) {
			continue
		}

		// Only report once per variable: check if this is the first block using it.
		if !isFirstUsage(vname, block, ctx) {
			continue
		}

		findings = append(findings, models.Finding{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Variable potentially uninitialized",
			Description: "Variable '" + vname + "' is used here but doesn't seem to be assigned anywhere in the flow.",
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Ensure the variable is initialized with a SET action or as an output of another action.",
			Metadata:    map[string]interface{}{"variable": vname},
		})
	}

	return findings
}

func isSystemVariable(vname string) bool {
	v := strings.ToLower(vname)
	systemVars := map[string]struct{}{
		// Date/time
		"currentdatetime": {}, "yesterday": {}, "tomorrow": {},
		// Loop iteration
		"loopindex": {}, "currentitem": {},
		// UI interactions
		"buttonpressed": {}, "selectedbutton": {},
		// Windows user/machine identity
		"username": {}, "currentuser": {}, "windowsusername": {}, "windowsuserdomainname": {},
		"computername": {}, "machinename": {}, "hostname": {},
		// File system paths
		"userprofile": {}, "desktopdirectory": {}, "programfiles": {},
		"programfilesx86": {}, "systemroot": {}, "windir": {}, "temp": {}, "tmp": {},
		// PAD primitive constants
		"newline": {}, "tab": {}, "true": {}, "false": {}, "blank": {},
		// Browser automation
		"pageurl": {}, "browserdata": {},
		// Error handling built-ins
		"lasterror": {}, "lasterrortext": {}, "errormessage": {}, "errortext": {},
		"errordescription": {}, "erroroccurred": {},
	}
	_, found := systemVars[v]
	return found
}

func isFirstUsage(vname string, block *models.Block, ctx *RuleContext) bool {
	// The earliest-reading block (lowest LineNumber) is precomputed once in
	// buildContext, so this is O(1). Previously this rescanned every reader of
	// the variable on each block, making the rule O(readers²) per variable —
	// the dominant cost of analysis on large flows.
	return ctx.FirstReaderByVar[vname] == block.ID
}

func isAssignedAnywhere(vname string, ctx *RuleContext) bool {
	// WritersByVar is pre-indexed from the _output and _var properties — the
	// same union this function previously derived by scanning every block.
	return len(ctx.WritersByVar[vname]) > 0
}
