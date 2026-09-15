package analyzer

import (
	"strings"

	"pad-core/models"
)

// CommandInjectionRiskRule flags shell/DOS-command actions that interpolate
// variables into the command line (System.RunDOSCommand with %var% args), a
// classic command-injection vector: a malicious value can append `& del /f` or
// chain another command. PAD's system-command actions should pass arguments as
// discrete parameters, not concatenate them into one command string.
type CommandInjectionRiskRule struct{}

func (r *CommandInjectionRiskRule) ID() string   { return "command-injection-risk" }
func (r *CommandInjectionRiskRule) Name() string { return "Command or script injection risk" }
func (r *CommandInjectionRiskRule) Description() string {
	return "System/DOS command and Scripting.* (PowerShell, VBScript, Python, JavaScript) actions that interpolate variables into the command or script text. A malicious value can chain additional commands or code (injection)."
}
func (r *CommandInjectionRiskRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *CommandInjectionRiskRule) Category() string                 { return "Security" }

// commandActionPrefixes — PAD actions that hand a string to an interpreter:
// DOS/shell commands and the Scripting.* family (RunPowershellScript,
// RunVBScript, RunPythonScript, RunJavascript). The scripting actions were
// matched by NO security rule — arbitrary code execution from an interpolated
// variable went unreported while a DOS command with the same variable was an
// Error.
var commandActionPrefixes = []string{
	"system.rundoscommand",
	"system.run",
	"cmd.",
	"scripting.",
}

func (r *CommandInjectionRiskRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	rawLower := strings.ToLower(block.RawType)
	isCmd := false
	for _, p := range commandActionPrefixes {
		if strings.HasPrefix(rawLower, p) {
			isCmd = true
			break
		}
	}
	if !isCmd {
		return nil
	}
	// A variable reference in the raw command line means caller data flows
	// into the shell. PAD variable refs are %Name%; %% is a literal percent
	// escape (not a variable), so match the same %VarName% pattern the
	// SanitizeCommandVarsPatch fixer targets — otherwise '100%% complete' is a
	// false-positive finding the fixer can't resolve.
	for _, val := range block.Properties {
		if sqlVarRef.MatchString(val) {
			return []models.Finding{{
				RuleID:      r.ID(),
				Severity:    r.DefaultSeverity(),
				Title:       "Command injection risk",
				Description: "A system command interpolates variables into the command line. A malicious value can chain additional shell commands.",
				BlockID:     block.ID,
				SubflowID:   block.SubflowID,
				Suggestion:  "Pass caller-supplied values as discrete command parameters (not concatenated into the command string) and validate/allow-list them before invocation.",
			}}
		}
	}
	return nil
}

func init() { registerRule(&CommandInjectionRiskRule{}) }
