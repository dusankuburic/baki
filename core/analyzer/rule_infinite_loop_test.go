package analyzer

import (
	"testing"

	"pad-core/models"
)

// TestInfiniteLoop_ExitDetectionIsExact pins the tightened exit detection: only
// a real loop-exit construct satisfies the rule. A property VALUE or an action
// name that merely contains "Exit" or "Break" used to count, so a loop whose
// body only displayed "Exit code received" was declared safe.
func TestInfiniteLoop_ExitDetectionIsExact(t *testing.T) {
	flagged := map[string]string{
		"message text mentioning Exit": "" +
			"LOOP WHILE %Flag% = True\n" +
			"    Display.ShowMessage Message: 'Exit code received'\n" +
			"END\n",
		"message text mentioning Break": "" +
			"LOOP WHILE %Flag% = True\n" +
			"    Display.ShowMessage Message: 'Break time'\n" +
			"END\n",
	}
	safe := map[string]string{
		"EXIT LOOP in the body": "" +
			"LOOP WHILE %Flag% = True\n" +
			"    EXIT LOOP\n" +
			"END\n",
		"EXIT LOOP under an IF": "" +
			"LOOP WHILE %Flag% = True\n" +
			"    IF %Done% = True THEN\n" +
			"        EXIT LOOP\n" +
			"    END\n" +
			"END\n",
	}
	for name, body := range flagged {
		t.Run(name, func(t *testing.T) {
			if infiniteLoopFindings(t, body) == 0 {
				t.Error("a loop with no real exit was NOT reported: prose containing 'Exit'/'Break' counted as an exit")
			}
		})
	}
	for name, body := range safe {
		t.Run(name, func(t *testing.T) {
			if n := infiniteLoopFindings(t, body); n != 0 {
				t.Errorf("a loop with a real EXIT LOOP was reported %d time(s)", n)
			}
		})
	}
}

// TestHasExitCondition_RawTypeSubstringNoLongerCounts uses hand-built blocks so
// the RawType is under test control: a RawType that merely contains "exit" is
// not an exit, while the parser's exact EXIT_LOOP / BREAK tokens are.
func TestHasExitCondition_RawTypeSubstringNoLongerCounts(t *testing.T) {
	rule := &InfiniteLoopRiskRule{}
	cases := []struct {
		name    string
		child   *models.Block
		wantHit bool
	}{
		{"action whose RawType contains exit", makeBlock("b2", "Get exit code", models.BlockTypeAction, "Process.GetExitCode", 4), true},
		{"action whose Name contains Break", makeBlock("b2", "Break down totals", models.BlockTypeAction, "Variables.SetVariable", 4), true},
		{"parser EXIT_LOOP token", makeBlock("b2", "Exit Loop", models.BlockTypeAction, "EXIT_LOOP", 4), false},
		{"BREAK token", makeBlock("b2", "Break", models.BlockTypeAction, "BREAK", 4), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop := makeBlock("b1", "Loop", models.BlockTypeLoop, "LOOP", 0)
			loop.SubflowID = "sf1"
			loop.Children = []models.Block{*tc.child}
			flow := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
			got := rule.Check(loop, buildContext(flow, nil))
			if (len(got) == 1) != tc.wantHit {
				t.Fatalf("findings = %d, want hit=%v", len(got), tc.wantHit)
			}
		})
	}
}

func TestIsExitLoopRawType(t *testing.T) {
	yes := []string{"EXIT LOOP", "EXIT_LOOP", "exit_loop", "ExitLoop", "BREAK", " break "}
	no := []string{"", "EXIT", "EXIT FUNCTION", "Process.GetExitCode", "Breakpoint", "Loop.Exit"}
	for _, s := range yes {
		if !isExitLoopRawType(s) {
			t.Errorf("%q should be an exit-loop raw type", s)
		}
	}
	for _, s := range no {
		if isExitLoopRawType(s) {
			t.Errorf("%q should NOT be an exit-loop raw type", s)
		}
	}
}
