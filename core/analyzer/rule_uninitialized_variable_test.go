package analyzer

import (
	"testing"

	"pad-core/models"
)

// TestIsSystemVariable_CaseHandling pins the allocation-free case handling
// in isSystemVariable: the direct (already-lowercase) map hit, the single
// ToLower fallback for mixed-case names, and — the part the fallback guard
// protects — misses that stay misses whether or not the name carries an
// uppercase byte. Semantics must match the old unconditional
// systemVarSet[strings.ToLower(vname)] lookup exactly.
func TestIsSystemVariable_CaseHandling(t *testing.T) {
	cases := []struct {
		vname string
		want  bool
	}{
		{"currentdatetime", true}, // direct hit, no ToLower needed
		{"loopindex", true},       // direct hit
		{"CurrentDateTime", true}, // mixed case → lowered retry hits
		{"LoopIndex", true},       // mixed case → lowered retry hits
		{"CURRENTDATETIME", true}, // all caps → lowered retry hits
		{"ghostvar", false},       // lowercase miss: guard skips the retry
		{"GhostVar", false},       // mixed-case miss: retry still misses
		{"", false},               // empty name: neither path can hit
	}
	for _, tc := range cases {
		if got := isSystemVariable(tc.vname); got != tc.want {
			t.Errorf("isSystemVariable(%q) = %v, want %v", tc.vname, got, tc.want)
		}
	}
}

// TestUninitializedVariableRule_InputPrefixCaseFold exercises the
// allocation-free input_ prefix test through the public Check path: the
// prefix must match case-insensitively in every spelling, because flows
// name their input variables freely (Input_UserName, input_username, …)
// and all of them are provided externally.
func TestUninitializedVariableRule_InputPrefixCaseFold(t *testing.T) {
	rule := &UninitializedVariableRule{}
	for _, vname := range []string{"Input_UserName", "input_username", "INPUT_USERNAME"} {
		b := makeBlock("b1", "Use input", models.BlockTypeAction, "SetVariable.Set", 0)
		b.Variables = []string{vname}
		b.SubflowID = "sf1"
		b.LineNumber = 1
		flow := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		if got := rule.Check(b, buildContext(flow, nil)); len(got) != 0 {
			t.Errorf("variable %q (input_ prefix) was flagged as uninitialized: %v", vname, got)
		}
	}
}
