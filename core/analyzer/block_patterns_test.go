package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// commentTerminatorFlow is an ordinary subflow whose leading COMMENT happens to
// contain the word "Returns". For a COMMENT block the parser stores the comment
// PROSE in Name, and terminatorNames contains "Return" — matched with
// strings.Contains — so the comment reads as a flow terminator and everything
// after it is reported unreachable.
const commentTerminatorFlow = "#Region \"Framework_GetTransactionData\"\n" +
	"COMMENT  Returns the next transaction item, or blank when the queue is empty\n" +
	"Excel.ReadFromExcel Instance: %ExcelInstance% Range: $'''A2:D2''' => TransactionItem\n" +
	"Variables.SetVariable NewValue: %TransactionItem% Name: Output_Item\n" +
	"Display.ShowMessageBox Message: $'''Fetched %Output_Item%'''\n" +
	"#EndRegion\n"

func findingsByRule(report *models.AnalysisReport, ruleID string) []models.Finding {
	var out []models.Finding
	for _, f := range report.Findings {
		if f.RuleID == ruleID {
			out = append(out, f)
		}
	}
	return out
}

// TestDeadCode_CommentIsNotATerminator is the regression gate for the worst
// defect in the analyzer: an Info-severity false positive wired to a
// DESTRUCTIVE fixer.
//
// `dead-code`'s auto-fix is `remove-block` (engine.go), so `bakicli fix` acts on
// this false positive by deleting the subflow's real body — three actions here.
// The finding costs the user their automation, not just a wrong line in a report.
func TestDeadCode_CommentIsNotATerminator(t *testing.T) {
	doc, err := parser.ParseText(commentTerminatorFlow, "Main.txt", int64(len(commentTerminatorFlow)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)

	if got := findingsByRule(report, "dead-code"); len(got) != 0 {
		t.Errorf("a COMMENT containing %q was treated as a flow terminator: %d dead-code finding(s); "+
			"first: %s", "Returns", len(got), got[0].Title)
	}
}

// TestFixLoop_DoesNotDeleteBodyAfterAComment is the same defect measured where
// it actually hurts. A test asserting only that the finding is absent would pass
// again the moment someone re-broke the fixer dispatch, so this one asserts the
// user's file survives.
func TestFixLoop_DoesNotDeleteBodyAfterAComment(t *testing.T) {
	src := commentTerminatorFlow
	before := strings.Count(src, "\n")

	if _, err := ApplyFixesToSource(&src, "Main.txt", FixOptions{Limit: 50}); err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}

	for _, must := range []string{
		"Excel.ReadFromExcel",
		"Variables.SetVariable",
		"Display.ShowMessageBox",
	} {
		if !strings.Contains(src, must) {
			t.Errorf("the fix loop deleted %q — a comment mentioning \"Returns\" made the rest of the "+
				"subflow look unreachable.\nresulting source:\n%s", must, src)
		}
	}
	if after := strings.Count(src, "\n"); after < before {
		t.Errorf("source shrank from %d lines to %d", before, after)
	}
}

// TestIsTerminator_MatchesTheActualConstructs pins the positive half: narrowing
// the match must not stop real terminators from being recognised, or `dead-code`
// silently stops working altogether.
func TestIsTerminator_MatchesTheActualConstructs(t *testing.T) {
	terminators := []models.Block{
		{RawType: "ExitSubflow"},
		{RawType: "TerminateFlow"},
		{Type: models.BlockTypeEnd},
		{Name: "Exit subflow"},
	}
	for _, b := range terminators {
		if !isTerminator(&b) {
			t.Errorf("isTerminator(%+v) = false, want true", b)
		}
	}

	notTerminators := []models.Block{
		{Type: models.BlockTypeComment, Name: "Returns the next item"},
		{Type: models.BlockTypeComment, Name: "End flow handling is below"},
		{RawType: "Excel.ReadFromExcel", Name: "Return values from the sheet"},
	}
	for _, b := range notTerminators {
		if isTerminator(&b) {
			t.Errorf("isTerminator(%+v) = true, want false", b)
		}
	}
}

// TestIsExitLoop_CommentDoesNotSuppressInfiniteLoop is the mirror image. The same
// substring match means a comment containing "Break" satisfies hasExitCondition,
// which SUPPRESSES a genuinely unbounded loop — a false negative on the rule that
// exists to catch the worst runtime failure a flow can have.
func TestIsExitLoop_CommentDoesNotSuppressInfiniteLoop(t *testing.T) {
	comment := models.Block{Type: models.BlockTypeComment, Name: "Break the loop when the queue drains"}
	if isExitLoop(&comment) {
		t.Error("a COMMENT mentioning \"Break\" counted as a loop exit, hiding an unbounded loop")
	}

	real := models.Block{RawType: "ExitLoop"}
	if !isExitLoop(&real) {
		t.Error("a real ExitLoop was not recognised")
	}
}

// TestTerminators_RealParsedConstructsStillDetected closes the loop on the risk
// this change carries: narrowing the match must not make `dead-code` stop
// working. These run through the actual parser rather than hand-built blocks, so
// they assert against whatever RawType/Name the exporter shape really produces.
func TestTerminators_RealParsedConstructsStillDetected(t *testing.T) {
	src := "#Region \"Main\"\n" +
		"Display.ShowMessageBox Message: $'''before'''\n" +
		"EXIT FUNCTION\n" +
		"Display.ShowMessageBox Message: $'''after'''\n" +
		"#EndRegion\n"

	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)

	if got := findingsByRule(report, "dead-code"); len(got) == 0 {
		t.Error("a real EXIT FUNCTION no longer marks the following block unreachable — " +
			"the matcher was narrowed too far and dead-code detection is now dead itself")
	}
}

// TestExitLoop_RealBreakStillEndsTheLoop is the same guard for isExitLoop: a real
// EXIT LOOP must still satisfy hasExitCondition, or infinite-loop-risk starts
// firing on correctly-bounded loops.
func TestExitLoop_RealBreakStillEndsTheLoop(t *testing.T) {
	src := "#Region \"Main\"\n" +
		"LOOP WHILE %Flag% = True\n" +
		"    Display.ShowMessageBox Message: $'''working'''\n" +
		"    EXIT LOOP\n" +
		"END\n" +
		"#EndRegion\n"

	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)

	if got := findingsByRule(report, "infinite-loop-risk"); len(got) != 0 {
		t.Errorf("a loop containing a real EXIT LOOP was reported as possibly infinite (%d finding(s)) — "+
			"isExitLoop no longer recognises the break", len(got))
	}
}
