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
// across iterations (defeating fixLoopStallLimit) until `limit` ran out. The
// fix loop must never apply more fixes than there are findings on entry —
// and its output must re-parse cleanly both times.
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
		if n > before {
			t.Errorf("pass %d: applied %d fixes but only %d fixable findings existed — the loop is manufacturing work (livelock)", pass, n, before)
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
