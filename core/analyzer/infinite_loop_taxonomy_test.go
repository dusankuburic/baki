package analyzer

import (
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

func infiniteLoopFindings(t *testing.T, body string) int {
	t.Helper()
	src := "#Region \"Main\"\n" + body + "#EndRegion\n"
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	rep := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	n := 0
	for _, f := range rep.Findings {
		if f.RuleID == "infinite-loop-risk" {
			n++
		}
	}
	return n
}

// TestInfiniteLoop_BoundedLoopsAreNotFlagged is the regression gate for the rule
// that single-handedly made the default CI gate unusable.
//
// infinite-loop-risk is Error severity — the GitHub Action's default fail-on
// threshold — and it judged every loop by hasExitCondition alone, a predicate
// that only understands `SET x TO y`. Loop KIND was discarded at parse time, so
// the rule could not tell a loop that terminates by construction from one that
// might not. On the repo's own sample corpus it produced 204 of the 204 total
// Error findings, all on bounded loops. Since a for-each is the most common
// construct in PAD, essentially every real flow failed the gate.
func TestInfiniteLoop_BoundedLoopsAreNotFlagged(t *testing.T) {
	cases := map[string]string{
		"foreach over a collection": "" +
			"LOOP FOREACH CurrentItem IN DataTable\n" +
			"    Display.ShowMessageBox Message: %CurrentItem%\n" +
			"END\n",
		"counted with a named counter": "" +
			"LOOP LoopIndex FROM 1 TO 10 STEP 1\n" +
			"    Display.ShowMessageBox Message: %LoopIndex%\n" +
			"END\n",
		// reLoopRange requires a counter NAME because it exists to extract that
		// variable; this anonymous form is bounded just the same.
		"counted without a named counter": "" +
			"LOOP FROM 2 TO %FreeRow% - 1 STEP 1\n" +
			"    Display.ShowMessageBox Message: 'row'\n" +
			"END\n",
		// PAD's idiomatic counter step is an ACTION, not a SET. It carries no
		// _value referencing the variable, so the progress check could never
		// see it.
		"while whose counter advances via Variables.IncreaseVariable": "" +
			"LOOP WHILE %Counter% < 5\n" +
			"    Variables.IncreaseVariable Value: 1 Name: Counter\n" +
			"END\n",
		"while whose counter advances via SET": "" +
			"LOOP WHILE %RetryCount% < 3\n" +
			"    SET RetryCount TO %RetryCount% + 1\n" +
			"END\n",
		"while with an explicit Exit Loop": "" +
			"LOOP WHILE %Flag% = True\n" +
			"    EXIT LOOP\n" +
			"END\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := infiniteLoopFindings(t, body); got != 0 {
				t.Errorf("a loop that terminates by construction was reported as possibly "+
					"infinite (%d finding(s)) — at Error severity this fails the default CI gate", got)
			}
		})
	}
}

// TestInfiniteLoop_StillCatchesTheRealDefect is the half that matters most. A
// rule that never fires is worse than one that fires wrongly: it looks like
// coverage while providing none. These loops genuinely may not terminate.
func TestInfiniteLoop_StillCatchesTheRealDefect(t *testing.T) {
	cases := map[string]string{
		"while with no progress at all": "" +
			"LOOP WHILE %Flag% = True\n" +
			"    Display.ShowMessageBox Message: 'still going'\n" +
			"END\n",
		// A constant reset is not progress: `WHILE %Found% = False` with
		// `SET Found TO False` in the body never terminates.
		"while whose condition variable is reset to a constant": "" +
			"LOOP WHILE %Found% = False\n" +
			"    SET Found TO False\n" +
			"END\n",
		"while whose body only writes an unrelated variable": "" +
			"LOOP WHILE %Counter% < 5\n" +
			"    Variables.IncreaseVariable Value: 1 Name: Unrelated\n" +
			"END\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := infiniteLoopFindings(t, body); got == 0 {
				t.Errorf("a loop that may genuinely run forever was NOT reported — the rule " +
					"has been widened into silence")
			}
		})
	}
}

// TestInfiniteLoop_KindIsRecordedOnTheBlock pins the parser half, so the
// taxonomy cannot be quietly dropped and leave the rule inferring again.
func TestInfiniteLoop_KindIsRecordedOnTheBlock(t *testing.T) {
	cases := map[string]string{
		"LOOP FOREACH x IN Coll\nEND\n": parser.LoopKindForEach,
		"LOOP i FROM 1 TO 10\nEND\n":    parser.LoopKindCounted,
		"LOOP FROM 1 TO 10\nEND\n":      parser.LoopKindCounted,
		"LOOP WHILE %x% = 1\nEND\n":     parser.LoopKindWhile,
	}
	for body, want := range cases {
		src := "#Region \"Main\"\n" + body + "#EndRegion\n"
		doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
		if err != nil {
			t.Fatalf("ParseText: %v", err)
		}
		var got string
		for i := range doc.Subflows[0].Blocks {
			if doc.Subflows[0].Blocks[i].Type == models.BlockTypeLoop {
				got = doc.Subflows[0].Blocks[i].Properties[parser.LoopKindProperty]
			}
		}
		if got != want {
			t.Errorf("%q classified as %q, want %q", body, got, want)
		}
	}
}
