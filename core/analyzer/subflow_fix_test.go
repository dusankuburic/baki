package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// uncalledSubflowFlow has a helper subflow nothing calls statically — the shape
// of every "run this by hand" or dynamically-invoked routine in a real flow
// library.
const uncalledSubflowFlow = "#Region \"Main\"\n" +
	"CALL ProcessOrders\n" +
	"#EndRegion\n" +
	"#Region \"Framework_TakeScreenshot\"\n" +
	"Workstation.TakeScreenshot File: %ShotPath%\n" +
	"File.Move Files: %ShotPath% Destination: %ArchiveDir%\n" +
	"Display.ShowMessageBox Message: $'''Screenshot saved'''\n" +
	"#EndRegion\n"

// TestUncalledSubflow_FixDoesNotStripTheSubflowOneActionAtATime is the
// regression gate for a fix that deletes the wrong thing, repeatedly.
//
// uncalled-subflow attaches its finding to subflow.Blocks[0] as a stand-in for
// "this subflow", but its fix type is remove-block, which deletes exactly that
// one block. The finding then re-fires on the NEW first block — with a different
// content fingerprint, because the fingerprint hashes RawType/Name/LineNumber —
// so neither the no-progress guard nor the stall guard recognises it as the same
// problem. Each deletion also removes that block's other findings, which resets
// the stall counter's minimum. The loop strips the subflow one action per
// iteration, up to the limit.
func TestUncalledSubflow_FixDoesNotStripTheSubflowOneActionAtATime(t *testing.T) {
	src := uncalledSubflowFlow
	if _, err := ApplyFixesToSource(&src, "Main.txt", FixOptions{Limit: 50}); err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}

	for _, must := range []string{
		"Workstation.TakeScreenshot",
		"File.Move",
		"Display.ShowMessageBox",
	} {
		if !strings.Contains(src, must) {
			t.Errorf("the fix loop deleted %q from an uncalled subflow — the finding stands in for the "+
				"whole subflow but the fix removes a single block, so it re-fires and strips the body "+
				"one action at a time.\nresult:\n%s", must, src)
		}
	}
}

// TestUncalledSubflow_IsStillReported keeps the rule's value: not offering an
// auto-fix must not silence the finding. A subflow nothing calls is worth
// telling the user about — whether to delete it needs knowledge the analyzer
// does not have (it may be invoked dynamically, or run by an operator), which is
// exactly why it is not auto-fixable.
func TestUncalledSubflow_IsStillReported(t *testing.T) {
	doc, err := parser.ParseText(uncalledSubflowFlow, "Main.txt", int64(len(uncalledSubflowFlow)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)

	found := findingsByRule(report, "uncalled-subflow")
	if len(found) == 0 {
		t.Fatal("uncalled-subflow no longer fires — removing the fix must not remove the finding")
	}
	if found[0].AutoFix != "" {
		t.Errorf("uncalled-subflow still advertises the auto-fix %q; deciding a subflow is unused "+
			"needs knowledge the analyzer does not have", found[0].AutoFix)
	}
	if found[0].Suggestion == "" {
		t.Error("a finding with no auto-fix must carry a suggestion telling the user what to do")
	}
}
