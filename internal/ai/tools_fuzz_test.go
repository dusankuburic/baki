package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"pad-core/analyzer"
	"pad-core/models"
)

// toolFuzzFixtureFiles are the tool tests whose inline JSON inputs seed FuzzExecuteTool (against every tool).
var toolFuzzFixtureFiles = []string{
	"tools_test.go",
	"tools_json_test.go",
	"tools_fixes_test.go",
	"tools_lineage_test.go",
	"tools_list_findings_page_test.go",
	"tools_knowledge_test.go",
	"tools_stage_test.go",
}

// fuzzToolAnalysis is the analysis the fuzzed tools see: a canned report with a fixable finding on b1, and the
// real lineage walk over the document.
type fuzzToolAnalysis struct{}

func (fuzzToolAnalysis) AnalyzeFlow(context.Context, *models.FlowDocument) (*models.AnalysisReport, error) {
	return fixReport("b1"), nil
}

func (fuzzToolAnalysis) GetVariableLineage(doc *models.FlowDocument, name string) (*models.VariableHistory, error) {
	return analyzer.BuildVariableLineage(doc, name), nil
}

// FuzzExecuteTool runs a registered tool, chosen by the first argument, on arbitrary input against
// toolFixtureDoc with every dependency wired (analysis, a stub fix applier that approves, a knowledge base), so
// each handler gets past its gates to its input decoding. Invariants: no panic, a return within fuzzBudget, a
// result no longer than maxToolResultBytes plus the truncation marker, and valid UTF-8 — what the model and
// the trail read, error results included.
func FuzzExecuteTool(f *testing.F) {
	specs := ToolSpecs()
	if len(specs) == 0 {
		f.Fatal("no registered tools")
	}
	// The last seed is over the result cap on its own (TestExecuteTool_ErrorResultsAreBounded).
	inputs := []string{"", "{}", "null", "[]", "{", `{"blockId":"b1"}`, `{"query":"Connect"}`,
		`{"query":"q","limit":` + strings.Repeat("9", maxToolResultBytes+1) + `}`}
	for _, spec := range specs {
		var s jsonSchema
		if err := json.Unmarshal(spec.Def.InputSchema, &s); err != nil {
			f.Fatalf("%s: %v", spec.Def.Name, err)
		}
		in, _ := json.Marshal(sampleOf(s))
		inputs = append(inputs, string(in))
	}
	for _, decls := range fixtureLiterals(f, toolFuzzFixtureFiles...) {
		for _, strs := range decls {
			for _, s := range strs {
				if strings.HasPrefix(strings.TrimSpace(s), "{") {
					inputs = append(inputs, s)
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, in := range inputs {
		if seen[in] {
			continue
		}
		seen[in] = true
		for i := range specs {
			f.Add(uint8(i), []byte(in))
		}
	}
	// truncateResult's longest output: the cap plus its marker.
	maxResult := len(truncateResult(strings.Repeat("x", maxToolResultBytes+1)))

	f.Fuzz(func(t *testing.T, which uint8, input []byte) {
		name := specs[int(which)%len(specs)].Def.Name
		tctx := ToolContext{
			Ctx:       context.Background(),
			Doc:       toolFixtureDoc(),
			Analysis:  fuzzToolAnalysis{},
			Fixes:     &stubFixApplier{ret: "APPLIED"},
			Knowledge: &fakeKnowledge{text: "- [policy.md] Retry SQL three times."},
		}
		var out string
		runWithin(t, fuzzBudget, func() { out = ExecuteTool(name, json.RawMessage(input), tctx) })
		if len(out) > maxResult {
			t.Errorf("%s: result is %d bytes, want at most %d", name, len(out), maxResult)
		}
		if !utf8.ValidString(out) {
			t.Errorf("%s: result is not valid UTF-8", name)
		}
		if t.Failed() {
			t.Logf("input: %q\nresult (first 300 bytes): %q", input, out[:min(len(out), 300)])
		}
	})
}

// Found while bounding FuzzExecuteTool's result (the mutator rarely grows an input past 6000 bytes, so the case is
// pinned here): error results bypassed the maxToolResultBytes cap. A decode error quotes the offending number
// whole ("cannot unmarshal number 999…"), so an 8000-digit limit made an 8 KB result, and an unknown tool's
// result quotes the model's name. Every result, error or not, goes through truncateResult.
func TestExecuteTool_ErrorResultsAreBounded(t *testing.T) {
	maxResult := len(truncateResult(strings.Repeat("x", maxToolResultBytes+1)))
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc(), Analysis: fuzzToolAnalysis{},
		Knowledge: &fakeKnowledge{text: "k"}}
	long := strings.Repeat("9", 2*maxToolResultBytes)
	for _, tc := range []struct{ name, input string }{
		{"search_flow", `{"query":"q","limit":` + long + `}`},
		{"search_knowledge", `{"query":"q","limit":` + long + `}`},
		{"no_such_tool_" + strings.Repeat("é", maxToolResultBytes), `{}`},
	} {
		out := ExecuteTool(tc.name, json.RawMessage(tc.input), tctx)
		if len(out) > maxResult || !utf8.ValidString(out) || !strings.HasPrefix(out, "error: ") {
			t.Errorf("%.40s: %d-byte result (valid UTF-8 %v), want an error of at most %d: %.120q", tc.name, len(out), utf8.ValidString(out), maxResult, out)
		}
	}
}
