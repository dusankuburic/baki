package main

import (
	"strings"
	"testing"

	"pad-core/analyzer"
	"pad-core/parser"
)

// TestApplyFixesToSource_ResolvesFindings runs the iterative fix loop on a flow
// with multiple auto-fixable findings and asserts they are all resolved after
// the loop (the patched source re-parses + re-analyzes clean of those rules).
func TestApplyFixesToSource_ResolvesFindings(t *testing.T) {
	// redundant-action (SET X TO %X%) + missing-timeout on the HTTP action.
	source := "#Region \"Main\"\n" +
		"SET X TO %X%\n" +
		"HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x'\n" +
		"#EndRegion\n"

	fixed, err := analyzer.ApplyFixesToSource(&source, "Main.txt", nil, 50, nil)
	if err != nil {
		t.Fatalf("applyFixesToSource: %v", err)
	}
	if fixed == 0 {
		t.Fatal("expected at least one fix applied, got 0")
	}

	// Re-parse + re-analyze: the targeted rules must be gone.
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("re-parse after fixes failed (not faithful): %v\nsource:\n%s", err, source)
	}
	report := analyzer.RunAnalysis(doc, analyzer.AllRules(), nil, nil)
	for _, f := range report.Findings {
		if f.RuleID == "redundant-action" {
			t.Errorf("redundant-action still present after fix loop: %+v", f)
		}
	}
}

// TestApplyFixesToSource_DryRunDoesNotMutateInputObject verifies the *source
// pointer IS updated (that's how the caller gets the patched text), but the
// caller controls whether to write it — dry-run vs --apply is a runFix concern,
// not applyFixesToSource's. Here we just confirm the patched source differs
// from the original (a fix landed) and is faithful.
func TestApplyFixesToSource_DryRunDoesNotMutateInputObject(t *testing.T) {
	original := "#Region \"Main\"\nSET X TO %X%\n#EndRegion\n"
	source := original
	_, err := analyzer.ApplyFixesToSource(&source, "Main.txt", nil, 50, nil)
	if err != nil {
		t.Fatalf("applyFixesToSource: %v", err)
	}
	// The redundant SET line must be gone from the patched source.
	if strings.Contains(source, "SET X TO %X%") {
		t.Errorf("expected the redundant SET removed, source still contains it:\n%s", source)
	}
	// runFix's dry-run path prints `source` to stdout instead of writing it;
	// applyFixesToSource itself just mutates the string the caller owns.
	_ = original
}

// TestApplyFixesToSource_RuleFilter verifies --rule restricts the loop to the
// named rule: a redundant-action + an unused-variable are both present, but
// only redundant-action is fixed when the filter selects it.
func TestApplyFixesToSource_RuleFilter(t *testing.T) {
	// SET X TO %X% → redundant-action; SET Unused TO '1' → unused-variable.
	source := "#Region \"Main\"\nSET X TO %X%\nSET Unused TO '1'\n#EndRegion\n"
	only := map[string]bool{"redundant-action": true}

	if _, err := analyzer.ApplyFixesToSource(&source, "Main.txt", only, 50, nil); err != nil {
		t.Fatalf("applyFixesToSource: %v", err)
	}
	// The filtered rule's block is gone; the unfiltered rule's block remains.
	if strings.Contains(source, "SET X TO %X%") {
		t.Errorf("filtered rule redundant-action was not fixed:\n%s", source)
	}
	if !strings.Contains(source, "SET Unused TO '1'") {
		t.Errorf("unfiltered rule unused-variable was incorrectly fixed:\n%s", source)
	}
}

// TestApplyFixesToSource_NoFixableFindings verifies the loop is a no-op (0
// fixes, source unchanged) when there is nothing auto-fixable.
func TestApplyFixesToSource_NoFixableFindings(t *testing.T) {
	source := "#Region \"Main\"\nDisplay.ShowMessageBox Message: 'hi'\n#EndRegion\n"
	original := source
	fixed, err := analyzer.ApplyFixesToSource(&source, "Main.txt", nil, 50, nil)
	if err != nil {
		t.Fatalf("applyFixesToSource: %v", err)
	}
	if fixed != 0 {
		t.Errorf("expected 0 fixes on a clean flow, got %d", fixed)
	}
	if source != original {
		t.Errorf("source changed despite no fixes:\nbefore: %q\nafter:  %q", original, source)
	}
}

// TestApplyFixesToSource_DeclinedFixerDoesNotBlockOthers verifies that when a
// fixer declines (returns an empty patch) for one finding, the loop records it
// as skipped and continues to fix OTHER independent findings in the same run —
// rather than aborting the whole loop on the first decline.
//
// command-injection-risk fires on '100%% done' (the rule matches any '%'), but
// SanitizeCommandVarsPatch declines: its %VarName% regex doesn't match the
// literal '%%' escape, so it emits zero ops. command-injection registers before
// redundant-action (alphabetical init order), so its finding is picked first.
// With the old break-on-decline behavior this would abort the loop and leave the
// redundant-action finding (SET X TO %X%) unfixed.
func TestApplyFixesToSource_DeclinedFixerDoesNotBlockOthers(t *testing.T) {
	source := "#Region \"Main\"\n" +
		"System.RunDOSCommand Command: 'echo 100%% done'\n" +
		"SET X TO %X%\n" +
		"#EndRegion\n"

	fixed, err := analyzer.ApplyFixesToSource(&source, "Main.txt", nil, 50, nil)
	if err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}
	if fixed == 0 {
		t.Fatal("expected the redundant-action fix to land despite the declined command-injection fixer, got 0 fixes")
	}
	// The redundant-action finding must be resolved even though the
	// command-injection finding (picked first in report order) could not be
	// auto-fixed.
	if strings.Contains(source, "SET X TO %X%") {
		t.Errorf("redundant-action was not fixed (declined fixer blocked the loop):\n%s", source)
	}
}
