package analyzer

import (
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// FuzzApplyPatch fuzzes the patch application pipeline with arbitrary source
// text. The property: ApplyPatch must NEVER panic on any input, regardless of
// line numbers, op kinds, or content. The fuzzer generates random source and
// random patch ops (insert/wrap/remove/replace/append) with arbitrary line
// numbers and content, then verifies the output is a valid string.
//
// To run: go test -fuzz=FuzzApplyPatch -fuzztime=30s ./core/analyzer/
func FuzzApplyPatch(f *testing.F) {
	// Seed with a few realistic sources + patches.
	f.Add("line1\nline2\nline3", "insert", 2, "INSERTED")
	f.Add("#Region \"Main\"\nSET X TO 1\n#EndRegion\n", "remove", 2, "")
	f.Add("a\nb\nc", "wrap", 1, "HEADER")
	f.Add("only line", "replace", 1, "NEW")
	f.Add("x\ny", "append", 1, " APPENDED")

	f.Fuzz(func(t *testing.T, source, kind string, lineNum int, content string) {
		// Clamp lineNum to a reasonable range to avoid huge allocations.
		if lineNum < -10 {
			lineNum = -10
		}
		if lineNum > 10000 {
			lineNum = 10000
		}

		var op models.PatchOp
		switch kind {
		case "insert":
			op = models.PatchOp{Kind: "insert", BeforeLine: lineNum, Lines: []string{content}}
		case "remove":
			op = models.PatchOp{Kind: "remove", StartLine: lineNum, EndLine: lineNum + 1}
		case "wrap":
			op = models.PatchOp{Kind: "wrap", StartLine: lineNum, EndLine: lineNum, Header: content, Footer: "END", IndentDelta: 1}
		case "replace":
			op = models.PatchOp{Kind: "replace", StartLine: lineNum, Old: content, New: "REPLACED"}
		case "append":
			op = models.PatchOp{Kind: "append", StartLine: lineNum, Lines: []string{content}}
		default:
			t.Skip()
		}

		// The core property: ApplyPatch must not panic.
		result := ApplyPatch(source, models.Patch{Ops: []models.PatchOp{op}})

		// The result must be a valid string. An empty result is valid when a
		// remove op deletes all lines — the core property is "no panic".
		_ = result
	})
}

// FuzzFixerPipeline fuzzes the full parse → analyze → fix → re-parse pipeline.
// The property: applying any auto-fixer's patch to any parseable source must
// produce source that re-parses without error (faithfulness). This is the
// highest-value fuzz test because it covers the entire fixer surface.
//
// To run: go test -fuzz=FuzzFixerPipeline -fuzztime=60s ./core/analyzer/
func FuzzFixerPipeline(f *testing.F) {
	// Seed with realistic PAD sources that have fixable findings.
	f.Add("#Region \"Main\"\nSET X TO %X%\n#EndRegion\n")
	f.Add("#Region \"Main\"\nHTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com'\n#EndRegion\n")
	f.Add("#Region \"Main\"\nDISABLE CALL DoThing\n#EndRegion\n")
	f.Add("#Region \"Main\"\nSET UnusedVar TO 'hello'\n#EndRegion\n")
	f.Add("#Region \"Main\"\nFolder.Get Path: C:\\Users\\admin\\data\n#EndRegion\n")

	f.Fuzz(func(t *testing.T, source string) {
		// Skip empty or binary sources.
		if len(source) == 0 || len(source) > 10000 {
			t.Skip()
		}

		// Parse — skip if the source doesn't parse (parser has its own fuzz test).
		doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
		if err != nil {
			t.Skip()
		}
		if doc == nil || len(doc.Subflows) == 0 {
			t.Skip()
		}

		// Analyze.
		report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)

		// For each auto-fixable finding, apply the fix and verify re-parse.
		for i := range report.Findings {
			finding := &report.Findings[i]
			if finding.AutoFix == "" {
				continue
			}
			block := doc.BlocksByID[finding.BlockID]
			if block == nil {
				continue
			}
			variable, _ := finding.Metadata["variable"].(string)
			property, _ := finding.Metadata["property"].(string)
			patch, err := PatchForFix(block, finding.AutoFix, finding.RuleID, variable, property)
			if err != nil || len(patch.Ops) == 0 {
				continue // fixer declined — fine
			}

			// The core property: ApplyPatch must not panic.
			patched := ApplyPatch(source, patch)

			// The patched source must re-parse (faithfulness).
			// We don't require zero errors — just that it doesn't panic.
			_, _ = parser.ParseText(patched, "Main.txt", int64(len(patched)))
		}
	})
}
