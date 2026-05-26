package analyzer

import (
	"strings"
	"pad-analyzer/internal/models"
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
	// Find the block with the lowest LineNumber that uses this variable.
	// LineNumber is globally unique within a document so this gives correct
	// document-order ordering even across nested sibling lists.
	lowestLine := -1
	lowestID := ""
	for id, b := range ctx.AllBlocks {
		for _, v := range b.Variables {
			if v == vname {
				if lowestLine < 0 || b.LineNumber < lowestLine {
					lowestLine = b.LineNumber
					lowestID = id
				}
				break
			}
		}
	}
	return lowestID == block.ID
}

func isAssignedAnywhere(vname string, ctx *RuleContext) bool {
	// Check all blocks in the document to see if any block outputs this variable.
	for _, b := range ctx.AllBlocks {
		if b.Properties == nil {
			continue
		}

		// Most actions store their output variable name in "_output"
		if out, ok := b.Properties["_output"]; ok && out == vname {
			return true
		}
		// SET actions often use "_var"
		if out, ok := b.Properties["_var"]; ok && out == vname {
			return true
		}
		
		// Some actions might store it in other fields depending on the parser.
		// For thoroughness, we check if vname is explicitly listed as a primary output.
	}
	return false
}
