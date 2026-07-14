package analyzer

import (
	"testing"

	"pad-core/models"
)

// TestPatchForFix verifies the fixType → patch dispatch routes each fixType to
// the right fixer and produces a patch whose op kind matches expectations, plus
// that an unknown fixType errors. Per-fixer correctness (faithful + effective)
// is covered by the round-trip gates in autofix_test.go; this test covers the
// dispatch table itself.
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
		{"insert-handler-log", "insert-handler-log", leaf, "", "", "", false, "insert"},
		{"init-variable", "init-variable", leaf, "", "MyVar", "", false, "insert"},
		{"insert-error-log", "insert-error-log", leaf, "", "", "", false, "insert"},
		{"replace-with-variable", "replace-with-variable", &models.Block{LineNumber: 1, Properties: map[string]string{"password": "hunter2"}}, "", "", "password", false, "replace"},
		{"wrap-in-retry", "wrap-in-retry", leaf, "", "", "", false, "wrap"},
		{"insert-exit-condition", "insert-exit-condition", &models.Block{LineNumber: 2, Children: []models.Block{{LineNumber: 3}}}, "", "", "", false, "insert"},
		{"remove-block", "remove-block", leaf, "", "", "", false, "remove"},
		{"parameterize-sql", "parameterize-sql", &models.Block{LineNumber: 1, Properties: map[string]string{"Sql": "SELECT * FROM t WHERE id = %X%"}}, "", "", "Sql", false, "replace"},
		{"suppress", "suppress", leaf, "missing-timeout", "", "", false, "insert"},
		{"unknown fixType errors", "bogus", leaf, "", "", "", true, ""},
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
