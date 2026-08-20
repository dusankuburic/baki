package analyzer

import (
	"fmt"
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// buildFixableSource renders a synthetic PAD source whose blocks carry many
// auto-fixable findings: each group pairs a redundant SET (redundant-action →
// remove-block) with an HTTP invoke (unhandled-error → wrap-error-handler,
// missing-timeout → set-timeout). Used by the apply-fix-loop benchmark so the
// loop performs a realistic number of parse → analyze → patch iterations.
func buildFixableSource(groups int) string {
	var b strings.Builder
	b.Grow(groups * 160)
	b.WriteString("#Region \"Main\"\n")
	for i := 0; i < groups; i++ {
		fmt.Fprintf(&b, "SET X%d TO %%X%d%%\n", i, i)
		fmt.Fprintf(&b, "HTTPClient.InvokeUrl Method: GET Url: 'https://api%d.example.com/x' => R%d\n", i, i)
	}
	b.WriteString("#EndRegion\n")
	return b.String()
}

// BenchmarkApplyFixesToSource measures the full iterative fix loop
// (parse → analyze → apply patch, repeated once per applied fix). This is the
// hot path behind "fix all" in the UI and `bakicli fix`, and the one place
// where every parser/analyzer constant-factor cost is multiplied by the number
// of fixes. Run with:
//
//	go test ./core/analyzer/ -bench ApplyFixesToSource -run x -benchtime 3x
func BenchmarkApplyFixesToSource(b *testing.B) {
	const groups = 40
	source := buildFixableSource(groups)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		src := source
		n, err := ApplyFixesToSource(&src, "Main.txt", nil, 500, nil)
		if err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			b.Fatal("benchmark fixture produced no fixes; it has rotted")
		}
	}
}

// BenchmarkApplyPatch isolates the textual patch application on a large source
// (ApplyPatch rewrites the whole line slice per call, so its cost scales with
// source size — the per-iteration cost under the fix loop).
func BenchmarkApplyPatch(b *testing.B) {
	const nLines = 10000
	line := "WebAutomation.Click.Click Text: 'button' : 'Click button'"
	var sb strings.Builder
	sb.Grow(nLines * (len(line) + 1))
	for i := 0; i < nLines; i++ {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	source := sb.String()
	patch := models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: nLines / 2,
		Lines:      []string{"    WAIT 1"},
	}}}
	b.SetBytes(int64(len(source)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ApplyPatch(source, patch)
	}
}

// TestBuildFixableSourceYieldsFixableFindings guards the benchmark fixture
// itself: if rule/fixer changes ever make the source produce no fixable
// findings, the benchmark would silently measure an empty loop.
func TestBuildFixableSourceYieldsFixableFindings(t *testing.T) {
	source := buildFixableSource(3)
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(doc.Subflows) == 0 || doc.Metadata.BlockCount < 6 {
		t.Fatalf("fixture parsed to too few blocks (%d) — parser or fixture rotted", doc.Metadata.BlockCount)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	fixable := 0
	for _, f := range report.Findings {
		if f.AutoFix != "" {
			fixable++
		}
	}
	if fixable < 3 {
		t.Fatalf("expected >= 3 fixable findings from fixture, got %d (rules changed?)", fixable)
	}
}

// TestApplyFixesToSource_StallGuardStopsOscillatingFixers is the regression
// gate for the fix-loop livelock: an insert whose indentation the parser won't
// nest (so its finding never resolves) used to alternate with remove-block
// duplicates up to `limit`, mangling the file — each insert shifted line
// numbers, so the per-signature no-progress guard never fired. The stall
// detector bounds consecutive non-shrinking iterations instead.
func TestApplyFixesToSource_StallGuardStopsOscillatingFixers(t *testing.T) {
	source := "#Region \"Main\"\n" +
		"SET X TO %X%\n" +
		"HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x' => R\n" +
		"WAIT 0\n" +
		"#EndRegion\n"
	n, err := ApplyFixesToSource(&source, "Main.txt", nil, 100, nil)
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if n > 10 {
		t.Errorf("loop ran away: %d fixes on a 5-line flow (stall guard failed)", n)
	}
	// The patched source must still re-parse cleanly (round-trip gate).
	if doc, perr := parser.ParseText(source, "Main.txt", int64(len(source))); perr != nil {
		_ = doc
		t.Fatalf("post-fix source no longer parses: %v\nsource:\n%s", perr, source)
	}
}
