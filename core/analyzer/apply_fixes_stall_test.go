package analyzer

import (
	"strings"
	"testing"

	"pad-core/parser"
)

// TestApplyFixesToSource_SecondPassOnFixedSource is the regression gate for
// the livelock that survived the count-based stall guard: running the loop a
// SECOND time over already-fixed output re-entered the
// duplicate-action/remove-block oscillation with a pool that grew and shrank
// across iterations (defeating fixLoopStallLimit) until `limit` ran out.
//
// The livelock signatures this gate pins:
//   - pass 1 terminates naturally (applied far below `limit`), its output
//     re-parses cleanly, and the fixable pool reaches ZERO — the runaway loop
//     left 3 findings stuck and re-manufactured 100 no-op "fixes".
//   - pass 2/3 apply NOTHING (quiescence): re-running the loop on its own
//     output must be a no-op.
//
// Note: pass 1 MAY legitimately apply more fixes than there were findings on
// entry — a fix can REVEAL a new finding (wrap-error-handler correctly nests
// the action inside the handler, which then trips error-swallow until
// insert-error-log runs; similarly wrap-in-retry can reveal
// infinite-loop-risk). A cascade that drives the pool monotonically to zero
// is the loop working as designed; manufacturing work forever is the bug.
func TestApplyFixesToSource_SecondPassOnFixedSource(t *testing.T) {
	src := "#Region \"Main\"\n" +
		"SET X TO %X%\n" +
		"HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x' => R\n" +
		"WAIT 0\n" +
		"#EndRegion\n"

	for pass := 1; pass <= 3; pass++ {
		before := countFixableFindings(t, src)
		n, err := ApplyFixesToSource(&src, "Main.txt", nil, 100, nil)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		after := countFixableFindings(t, src)
		t.Logf("pass %d: applied=%d findings %d -> %d", pass, n, before, after)
		if pass == 1 {
			// Terminated naturally (nowhere near the 100 limit) and drove the
			// fixable pool to zero — the livelock ran to the limit with 3
			// findings still firing.
			if n >= 100 {
				t.Errorf("pass 1: applied %d fixes — ran to `limit` (livelock)", n)
			}
			if after != 0 {
				t.Errorf("pass 1: %d fixable findings remain (want 0; the livelock stranded 3)", after)
			}
		} else {
			// Quiescence: a second/third pass over fixed output must find no
			// work at all.
			if n != 0 || after != 0 {
				t.Errorf("pass %d: re-run manufactured %d fixes, %d findings remain (livelock)", pass, n, after)
			}
		}
		if _, perr := parser.ParseText(src, "Main.txt", int64(len(src))); perr != nil {
			t.Fatalf("pass %d: output no longer parses: %v\n%s", pass, perr, src)
		}
	}
}

func countFixableFindings(t *testing.T, src string) int {
	t.Helper()
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), nil, nil)
	n := 0
	for _, f := range report.Findings {
		if f.AutoFix != "" {
			n++
		}
	}
	return n
}

// TestApplyFixesToSource_NestedHandlerOscillation pins the specific shape:
// ON BLOCK ERROR with mis-indented children produces duplicate-action
// findings whose removal regrows (the parser re-nests differently), cycling
// the fixable pool size up/down. Even here the loop must terminate well
// below `limit`.
func TestApplyFixesToSource_NestedHandlerOscillation(t *testing.T) {
	src := "#Region \"Main\"\n" +
		"LOOP WHILE %RetryCount% < 3\n" +
		"    Display.ShowMessageBox Message: 'Error occurred'\n" +
		"END\n" +
		"WAIT 0\n" +
		"#EndRegion\n"
	n, err := ApplyFixesToSource(&src, "Main.txt", nil, 100, nil)
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if n > 20 {
		t.Errorf("oscillating fixture ran away: %d fixes (limit 100)", n)
	}
	if strings.Count(src, "Error occurred") > 5 {
		t.Errorf("insert loop accumulated %d handler-log lines:\n%s", strings.Count(src, "Error occurred"), src)
	}
}
