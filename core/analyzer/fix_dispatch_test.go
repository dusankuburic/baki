package analyzer

import (
	"reflect"
	"testing"

	"pad-core/models"
)

// TestPatchForFix verifies the fixType → patch dispatch routes each fixType to
// the right fixer and produces a patch whose op kind matches expectations, plus
// that an unknown fixType errors. Per-fixer correctness (faithful + effective)
// is covered by the round-trip gates in autofix_test.go; this test covers the
// fix registry itself, and every registered fix type must have a case here.
func TestPatchForFix(t *testing.T) {
	leaf := &models.Block{ID: "b1", Name: "Act", Type: models.BlockTypeAction, RawType: "HTTPClient.InvokeUrl", LineNumber: 1, Indent: 0, Properties: map[string]string{}, Variables: []string{}}
	cases := []struct {
		name     string
		fixType  string
		block    *models.Block
		ruleID   string
		variable string
		property string
		wantErr  bool
		wantKind string // first op kind; "" ⇒ don't check (patch may be empty/conditional)
	}{
		{"wrap-error-handler", "wrap-error-handler", leaf, "", "", "", false, "wrap"},
		{"insert-close", "insert-close", &models.Block{RawType: "Excel.LaunchExcel", LineNumber: 1, Properties: map[string]string{"_output": "ExcelInstance"}}, "", "", "", false, "insert"},
		{"set-timeout", "set-timeout", leaf, "", "", "", false, "append"},
		{"insert-delay", "insert-delay", leaf, "", "", "", false, "insert"},
		{"insert-delay-in-loop", "insert-delay-in-loop", &models.Block{LineNumber: 2, Indent: 0, Children: []models.Block{{LineNumber: 3}}}, "", "", "", false, "insert"},
		{"insert-handler-log", "insert-handler-log", leaf, "", "", "", false, "insert"},
		{"init-variable", "init-variable", leaf, "", "MyVar", "", false, "insert"},
		{"insert-error-log", "insert-error-log", leaf, "", "", "", false, "insert"},
		{"replace-with-variable", "replace-with-variable", &models.Block{LineNumber: 1, Properties: map[string]string{"password": "hunter2"}}, "", "", "password", false, "replace"},
		// "insert", not "wrap": wrap-in-retry now appends PAD's native
		// `ON ERROR REPEAT N TIMES WAIT M` instead of wrapping the action in a
		// counting loop, which repeated the action unconditionally three times.
		{"wrap-in-retry", "wrap-in-retry", leaf, "", "", "", false, "insert"},
		{"insert-exit-condition", "insert-exit-condition", &models.Block{LineNumber: 2, Children: []models.Block{{LineNumber: 3}}}, "", "", "", false, "insert"},
		{"remove-block", "remove-block", leaf, "", "", "", false, "remove"},
		{"parameterize-sql", "parameterize-sql", &models.Block{LineNumber: 1, Properties: map[string]string{"Sql": "SELECT * FROM t WHERE id = %X%"}}, "", "", "Sql", false, "replace"},
		{"append-output", "append-output", leaf, "", "", "", false, "append"},
		{"mask-sensitive-variable", "mask-sensitive-variable", &models.Block{LineNumber: 1, Properties: map[string]string{"password": "secret"}}, "", "Password", "", false, "replace"},
		{"insert-default", "insert-default", &models.Block{LineNumber: 1, Indent: 0}, "", "", "", false, "insert"},
		{"upgrade-to-https", "upgrade-to-https", &models.Block{LineNumber: 1, Properties: map[string]string{"url": "http://example.com/api"}}, "", "", "url", false, "replace"},
		{"sanitize-command-vars", "sanitize-command-vars", &models.Block{LineNumber: 1, Properties: map[string]string{"command": "echo %UserInput%"}}, "", "", "command", false, "replace"},
		{"suppress", "suppress", leaf, "missing-timeout", "", "", false, "insert"},
		{"unknown fixType errors", "bogus", leaf, "", "", "", true, ""},
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.fixType] = true
	}
	for _, spec := range FixTypes() {
		if !covered[spec.Type] {
			t.Errorf("registered fix type %q has no TestPatchForFix case", spec.Type)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := PatchForFix(tc.block, tc.fixType, tc.ruleID, tc.variable, tc.property)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for fixType %q, got patch: %+v", tc.fixType, patch)
				}
				return
			}
			if err != nil {
				t.Fatalf("PatchForFix(%q): unexpected error: %v", tc.fixType, err)
			}
			if tc.wantKind != "" {
				if len(patch.Ops) == 0 {
					t.Fatalf("expected op kind %q, got empty patch for %q", tc.wantKind, tc.fixType)
				}
				if patch.Ops[0].Kind != tc.wantKind {
					t.Errorf("first op kind = %q, want %q (fixType %q)", patch.Ops[0].Kind, tc.wantKind, tc.fixType)
				}
			}
		})
	}
}

// TestFixFlags_DeclareWhatTheBuilderReads keeps the Uses* flags honest: a
// builder whose patch changes with the variable, property or rule ID argument
// must declare that it reads it, and a declared argument must matter for some
// block (the frontend decides which finding metadata to send from them).
func TestFixFlags_DeclareWhatTheBuilderReads(t *testing.T) {
	blocks := []*models.Block{
		{ID: "b1", RawType: "Database.ExecuteSqlStatement", LineNumber: 2, Indent: 4,
			Properties: map[string]string{"Statement": "SELECT %A% FROM t", "Password": "hunter2", "_output": "Out"},
			Children:   []models.Block{{LineNumber: 3}}},
		{ID: "b2", RawType: "Excel.LaunchExcel", LineNumber: 1, Properties: map[string]string{"_output": "ExcelInstance"}},
	}
	args := []struct {
		flag FixFlag
		name string
		vary func(in FixInput) FixInput
	}{
		{FixUsesVariable, "variable", func(in FixInput) FixInput { in.Variable = "Other"; return in }},
		{FixUsesProperty, "property", func(in FixInput) FixInput { in.Property = "Password"; return in }},
		{FixUsesRuleID, "rule ID", func(in FixInput) FixInput { in.RuleID = "other-rule"; return in }},
	}
	for _, spec := range FixTypes() {
		for _, a := range args {
			matters := false
			for _, b := range blocks {
				in := FixInput{Block: b, RuleID: "some-rule", Variable: "Pwd", Property: "Statement"}
				if !reflect.DeepEqual(spec.Build(in), spec.Build(a.vary(in))) {
					matters = true
				}
			}
			declared := spec.Flags&a.flag != 0
			if matters && !declared {
				t.Errorf("fix %q reads the %s but does not declare it", spec.Type, a.name)
			}
			if declared && !matters {
				t.Errorf("fix %q declares it reads the %s, but its patch never depends on it", spec.Type, a.name)
			}
		}
	}
}

// TestIsBehaviourChangingFix_Exact pins the opt-in set (unsafe_fixes_test.go
// covers the fix loop honouring it).
func TestIsBehaviourChangingFix_Exact(t *testing.T) {
	want := map[string]bool{"sanitize-command-vars": true, "mask-sensitive-variable": true, "replace-with-variable": true}
	for _, spec := range FixTypes() {
		if got := IsBehaviourChangingFix(spec.Type); got != want[spec.Type] {
			t.Errorf("IsBehaviourChangingFix(%q) = %v, want %v", spec.Type, got, want[spec.Type])
		}
	}
	if IsBehaviourChangingFix("bogus") {
		t.Error("an unknown fix type must not be behaviour-changing")
	}
}
