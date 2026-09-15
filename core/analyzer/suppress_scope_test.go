package analyzer

import (
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

func countRule(fs []models.Finding, rule string) int {
	n := 0
	for _, f := range fs {
		if f.RuleID == rule {
			n++
		}
	}
	return n
}

func parseFlow(t *testing.T, src string) *models.FlowDocument {
	t.Helper()
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	return doc
}

// TestInlineSuppression_FileHeaderSuppressesParseError drives the real parser
// and engine: a `# pad-ignore[parse-error]` on the first line of the file
// silences the parse errors (the per-block rule anchors them on Blocks[0] —
// the directive comment itself, which the next-block scope can never name),
// counts them in Stats.Suppressed, and leaves every other finding alone.
func TestInlineSuppression_FileHeaderSuppressesParseError(t *testing.T) {
	const body = "WebAutomation.Click Element: 'btn'\nELSE\n" // ELSE outside IF is a parse error
	plain := "#Region \"Main\"\n" + body + "#EndRegion\n"
	withDirective := "#Region \"Main\"\n# pad-ignore[parse-error]\n" + body + "#EndRegion\n"

	base := RunAnalysis(parseFlow(t, plain), AllRules(), models.DefaultSettings(), nil)
	parseErrs := countRule(base.Findings, "parse-error")
	if parseErrs == 0 {
		t.Fatalf("fixture must produce a parse-error finding; got %v", ruleIDs(base.Findings))
	}

	rep := RunAnalysis(parseFlow(t, withDirective), AllRules(), models.DefaultSettings(), nil)
	if n := countRule(rep.Findings, "parse-error"); n != 0 {
		t.Fatalf("top-of-file directive did not suppress parse-error (%d left): %+v", n, rep.Findings)
	}
	if rep.Stats.Suppressed < 1 {
		t.Errorf("Stats.Suppressed = %d, want >= 1 (counted per finding)", rep.Stats.Suppressed)
	}
	if want := len(base.Findings) - parseErrs; len(rep.Findings) != want {
		t.Errorf("unrelated findings changed: got %d (%v), want %d", len(rep.Findings), ruleIDs(rep.Findings), want)
	}
}

// TestInlineSuppression_FileScopeCoversDocumentLevelFallback: when the first
// subflow has no blocks the engine reports parse errors with an empty BlockID
// against that subflow. The top of the file is the header of the first subflow
// that HAS blocks, and it must reach those block-less findings.
func TestInlineSuppression_FileScopeCoversDocumentLevelFallback(t *testing.T) {
	cmt := makeBlock("c1", "pad-ignore[parse-error]", models.BlockTypeComment, "COMMENT", 0)
	act := makeBlock("b1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
	act.SubflowID = "sf2"
	newFlow := func(blocks ...models.Block) *models.FlowDocument {
		return &models.FlowDocument{
			ID:          "t",
			ParseErrors: []models.ParseError{{Line: 1, Message: "malformed line", Severity: "error"}},
			Subflows: []models.Subflow{
				{ID: "sf1", Name: "Main"},
				{ID: "sf2", Name: "Helper", Blocks: blocks},
			},
		}
	}

	base := RunAnalysis(newFlow(*act), AllRules(), nil, nil)
	if countRule(base.Findings, "parse-error") == 0 {
		t.Fatal("fixture must exercise the document-level parse-error fallback")
	}
	for _, f := range base.Findings {
		if f.RuleID == "parse-error" && f.BlockID != "" {
			t.Fatalf("fallback finding should have no block, got %+v", f)
		}
	}

	rep := RunAnalysis(newFlow(*cmt, *act), AllRules(), nil, nil)
	if n := countRule(rep.Findings, "parse-error"); n != 0 {
		t.Fatalf("file-scope directive did not suppress the block-less parse-error (%d left)", n)
	}
	if rep.Stats.Suppressed != countRule(base.Findings, "parse-error") {
		t.Errorf("Stats.Suppressed = %d, want %d", rep.Stats.Suppressed, countRule(base.Findings, "parse-error"))
	}
}

// TestInlineSuppressions_Scopes pins the three scopes against synthetic
// findings: next-block (unchanged), subflow header, and file header.
func TestInlineSuppressions_Scopes(t *testing.T) {
	c := func(id, text string) models.Block {
		return *makeBlock(id, text, models.BlockTypeComment, "COMMENT", 0)
	}
	a := func(id string) models.Block {
		return *makeBlock(id, "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
	}
	flow := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{
		{ID: "sf1", Name: "Main", Blocks: []models.Block{c("c1", "pad-ignore[parse-error]"), a("a1")}},
		{ID: "sf2", Name: "Helper", Blocks: []models.Block{c("c2", "pad-ignore[empty-subflow]"), a("a2")}},
		{ID: "sf3", Name: "Late", Blocks: []models.Block{a("a3"), c("c3", "pad-ignore[uncalled-subflow]"), a("a4")}},
	}}
	supp := collectInlineSuppressions(flow)

	cases := []struct {
		name          string
		rule, blk, sf string
		want          bool
	}{
		{"subflow header covers its block-less findings", "parse-error", "", "sf1", true},
		{"file header reaches block-less findings in any subflow", "parse-error", "", "sf3", true},
		{"file header reaches document-level findings", "parse-error", "", "", true},
		{"file header is rule-specific", "empty-subflow", "", "sf1", false},
		{"non-first subflow header covers its own block-less findings", "empty-subflow", "", "sf2", true},
		{"non-first subflow header is not the file scope", "empty-subflow", "", "sf3", false},
		{"directive after a real block is not a header", "uncalled-subflow", "", "sf3", false},
		{"directive after a real block still targets the next block", "uncalled-subflow", "a4", "sf3", true},
		{"header directive covers a finding anchored on the directive comment", "parse-error", "c1", "sf1", true},
		{"header directive still targets the next block", "parse-error", "a1", "sf1", true},
		{"header directive does not widen to other rules", "unhandled-error", "a1", "sf1", false},
		{"blocks outside any scope are untouched", "parse-error", "a2", "sf2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := models.Finding{RuleID: tc.rule, BlockID: tc.blk, SubflowID: tc.sf}
			if got := supp.suppresses(&f); got != tc.want {
				t.Errorf("suppresses(%+v) = %v, want %v", f, got, tc.want)
			}
		})
	}

	// applyInlineSuppressions counts per finding.
	findings := []models.Finding{
		{RuleID: "parse-error", SubflowID: "sf1"},
		{RuleID: "parse-error", SubflowID: "sf1"},
		{RuleID: "unhandled-error", BlockID: "a1", SubflowID: "sf1"},
	}
	kept, n := applyInlineSuppressions(findings, supp)
	if n != 2 || len(kept) != 1 || kept[0].RuleID != "unhandled-error" {
		t.Errorf("applyInlineSuppressions: kept=%+v suppressed=%d", kept, n)
	}
}

// TestInlineSuppressions_BareDirectiveInHeader: a bare `# pad-ignore` at the
// top of a subflow silences every block-less finding of that subflow (and, at
// the top of the file, of the document), matching its next-block meaning.
func TestInlineSuppressions_BareDirectiveInHeader(t *testing.T) {
	flow := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{
		{ID: "sf1", Name: "Main", Blocks: []models.Block{
			*makeBlock("c1", "pad-ignore", models.BlockTypeComment, "COMMENT", 0),
			*makeBlock("a1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0),
		}},
	}}
	supp := collectInlineSuppressions(flow)
	for _, f := range []models.Finding{
		{RuleID: "parse-error", SubflowID: "sf1"},
		{RuleID: "anything", SubflowID: ""},
		{RuleID: "anything", BlockID: "a1", SubflowID: "sf1"},
	} {
		if !supp.suppresses(&f) {
			t.Errorf("bare header directive should suppress %+v", f)
		}
	}
	if supp.empty() {
		t.Error("empty() must be false once a directive exists")
	}
	if (&inlineSuppressions{}).suppresses(&models.Finding{RuleID: "x"}) || !(*inlineSuppressions)(nil).empty() {
		t.Error("an empty/nil suppression set must suppress nothing")
	}
}
