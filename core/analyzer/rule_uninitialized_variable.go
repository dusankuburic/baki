package analyzer

import (
	"pad-core/models"
	"strings"
)

type UninitializedVariableRule struct{}

func (r *UninitializedVariableRule) ID() string   { return "uninitialized-variable" }
func (r *UninitializedVariableRule) Name() string { return "Variable used before being initialized" }
func (r *UninitializedVariableRule) Description() string {
	return "Detects variables that are referenced but never assigned in the flow."
}
func (r *UninitializedVariableRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *UninitializedVariableRule) Category() string                 { return "Logic" }

func (r *UninitializedVariableRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	// nil until a finding is appended: most (block, rule) pairs produce none,
	// and the empty non-nil literal allocated one anyway. Callers treat nil
	// and empty identically (len/append).
	var findings []models.Finding

	// Variables used in this block (extracted by parser from %...%)
	used := block.Variables
	if len(used) == 0 {
		return nil
	}

	for _, vname := range used {
		// Heuristic: an Input variable is provided externally. Allocation-free
		// prefix test: this runs once per variable reference across the flow,
		// and ToLower copied every referenced name just for the comparison.
		if len(vname) >= 6 && strings.EqualFold(vname[:6], "input_") {
			continue
		}

		// Heuristic: common system variable (CurrentDateTime, LoopIndex, …).
		if isSystemVariable(vname) {
			continue
		}

		if isAssignedAnywhere(vname, ctx) {
			continue
		}

		// Report only once per variable: only its first usage fires.
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

// systemVarSet is the set of PAD built-in system variables (lowercased).
// Package-level: the per-call map literal allocated ~35 entries every check.
var systemVarSet = map[string]struct{}{
	"currentdatetime": {}, "yesterday": {}, "tomorrow": {},
	"loopindex": {}, "currentitem": {},
	"buttonpressed": {}, "selectedbutton": {},
	"username": {}, "currentuser": {}, "windowsusername": {}, "windowsuserdomainname": {},
	"computername": {}, "machinename": {}, "hostname": {},
	"userprofile": {}, "desktopdirectory": {}, "programfiles": {},
	"programfilesx86": {}, "systemroot": {}, "windir": {}, "temp": {}, "tmp": {},
	"newline": {}, "tab": {}, "true": {}, "false": {}, "blank": {},
	"pageurl": {}, "browserdata": {},
	"lasterror": {}, "lasterrortext": {}, "errormessage": {}, "errortext": {},
	"errordescription": {}, "erroroccurred": {},
}

func isSystemVariable(vname string) bool {
	// Try the direct map hit first (the common case is already lowercase); the
	// ToLower retry runs only on a miss AND only when the name contains an
	// ASCII uppercase byte — the only bytes whose lowering can turn a miss
	// into a hit against this all-lowercase ASCII key set. Zero allocations
	// for already-lowercase names.
	if _, found := systemVarSet[vname]; found {
		return true
	}
	if !hasUppercaseASCII(vname) {
		return false
	}
	_, found := systemVarSet[strings.ToLower(vname)]
	return found
}

// hasUppercaseASCII reports whether s contains an ASCII uppercase letter — a
// byte loop rather than `strings.ToLower(s) != s` so the guard never allocates
// (bytes >= 0x80 never fall in 'A'..'Z').
func hasUppercaseASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

func isFirstUsage(vname string, block *models.Block, ctx *RuleContext) bool {
	// The earliest-reading block (lowest LineNumber) is precomputed once in
	// buildContext, so this is O(1); the old reader rescan made the rule
	// O(readers²) per variable.
	return ctx.FirstReaderByVar[vname] == block.ID
}

func isAssignedAnywhere(vname string, ctx *RuleContext) bool {
	// WritersByVar is pre-indexed from the _output and _var properties — the
	// same union this function previously derived by scanning every block.
	return len(ctx.WritersByVar[vname]) > 0
}

func init() {
	registerRule(&UninitializedVariableRule{}, RuleMeta{
		Confidence:   models.ConfidenceLow,
		AutoFix:      "init-variable",
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
