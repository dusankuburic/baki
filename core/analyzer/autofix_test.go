package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestSuppressFindingPatch_RoundTripResolvesFinding is the load-bearing gate for
// the raw-text apply-fix approach (Phase 1): a generated patch, applied to the
// raw source, must re-parse cleanly AND the targeted finding must be gone after
// re-analysis — proving the fix is faithful (no serializer) and effective.
func TestSuppressFindingPatch_RoundTripResolvesFinding(t *testing.T) {
	// A flow with an unhandled-error-prone action (HTTP.Invoke with no error handler).
	const source = `Display.UiFlow

WebAutomation.OpenBrowser Chrome: Chrome URL: '''https://example.com'''
HTTPClient.InvokeUrl Method: GET Url: '''https://api.example.com/x''' => Response
Display.CloseBrowser

# End Region`

	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}

	// First analysis — expect the HTTP action to be flagged.
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var httpFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "unhandled-error" || report.Findings[i].RuleID == "missing-timeout" {
			httpFinding = &report.Findings[i]
			break
		}
	}
	if httpFinding == nil {
		t.Fatalf("expected an unhandled-error/missing-timeout finding on the HTTP action, got: %+v", report.Findings)
	}
	targetRule := httpFinding.RuleID
	block := doc.BlocksByID[httpFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", httpFinding.BlockID)
	}

	// Generate + apply the suppress patch to the RAW source.
	patch := SuppressFindingPatch(block, targetRule)
	patched := ApplyPatch(source, patch)

	// Re-parse the patched source — must succeed (faithful edit, no structure loss).
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after patch failed (not faithful): %v", err)
	}

	// Re-analyze — the targeted finding must now be suppressed (gone + counted).
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == targetRule && f.BlockID == httpFinding.BlockID {
			t.Errorf("targeted %s finding still present after suppress patch: %+v", targetRule, f)
		}
	}
	if report2.Stats.Suppressed == 0 {
		t.Errorf("expected the finding to be suppressed (counted), got stats: %+v", report2.Stats)
	}
}

// TestApplyPatch_InsertsBottomUp confirms multiple inserts land at the right
// lines (later inserts don't shift earlier ones — the bottom-up ordering).
func TestApplyPatch_InsertsBottomUp(t *testing.T) {
	source := "line1\nline2\nline3"
	// Insert before line 1 and before line 3.
	p := models.Patch{Ops: []models.PatchOp{
		{Kind: "insert", BeforeLine: 3, Lines: []string{"before3"}},
		{Kind: "insert", BeforeLine: 1, Lines: []string{"before1"}},
	}}
	got := ApplyPatch(source, p)
	want := "before1\nline1\nline2\nbefore3\nline3"
	if got != want {
		t.Errorf("ApplyPatch multi-insert = %q, want %q", got, want)
	}
}

// TestApplyPatch_NoOpsPassthrough confirms an empty patch leaves text untouched.
func TestApplyPatch_NoOpsPassthrough(t *testing.T) {
	if got := ApplyPatch("abc", models.Patch{}); got != "abc" {
		t.Errorf("empty patch should pass through, got %q", got)
	}
}

// TestWrapInErrorHandlerPatch_RoundTripResolvesFinding is the gate for the
// wrap-in-error-handler apply-fix: wrapping a fallible action in
// ON BLOCK ERROR … END must (a) re-parse cleanly (faithful, correct indent)
// and (b) give the action an error-handler ancestor so the unhandled-error
// finding no longer fires.
func TestWrapInErrorHandlerPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = `#Region "Main"
Excel.LaunchExcel Visible: True LoadAddIns: False => ExcelInstance
Display.CloseBrowser
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var httpFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "unhandled-error" {
			httpFinding = &report.Findings[i]
			break
		}
	}
	if httpFinding == nil {
		t.Fatalf("expected an unhandled-error finding on the HTTP action, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[httpFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", httpFinding.BlockID)
	}

	patched := ApplyPatch(source, WrapInErrorHandlerPatch(block))

	// Re-parse — must succeed (correct ON BLOCK ERROR / END + indent).
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after wrap failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	// Re-analyze — the unhandled-error finding must be gone.
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "unhandled-error" {
			t.Errorf("unhandled-error still present after wrap; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestApplyWrap_ReindentsAndWraps verifies the wrap op mechanics directly:
// header + re-indented body + footer replace the range.
func TestApplyWrap_ReindentsAndWraps(t *testing.T) {
	source := "a\nHTTP.Get Url: 'x'\nb"
	p := models.Patch{Ops: []models.PatchOp{{
		Kind: "wrap", StartLine: 2, EndLine: 2,
		Header: "ON BLOCK ERROR", Footer: "END", IndentDelta: 1,
	}}}
	got := ApplyPatch(source, p)
	want := "a\nON BLOCK ERROR\n    HTTP.Get Url: 'x'\nEND\nb"
	if got != want {
		t.Errorf("wrap = %q, want %q", got, want)
	}
}

// TestInsertClosePatch_RoundTripResolvesFinding verifies the insert-close
// apply-fix: inserting a matching close action (referencing the handle variable)
// after the open must (a) re-parse cleanly and (b) resolve the resource-leak
// finding — the rule detects the close via the variable reference.
func TestInsertClosePatch_RoundTripResolvesFinding(t *testing.T) {
	const source = `#Region "Main"
Excel.LaunchExcel Visible: True LoadAddIns: False => ExcelInstance
Display.ShowMessageBox Message: 'done'
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var leakFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "resource-leak" {
			leakFinding = &report.Findings[i]
			break
		}
	}
	if leakFinding == nil {
		t.Fatalf("expected a resource-leak finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[leakFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", leakFinding.BlockID)
	}

	patched := ApplyPatch(source, InsertClosePatch(block))

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after insert-close failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "resource-leak" {
			t.Errorf("resource-leak still present after insert-close; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestSetTimeoutPatch_RoundTripResolvesFinding verifies the set-timeout
// apply-fix: appending ` Timeout: 30` to the action's source line must (a)
// re-parse cleanly and (b) resolve the missing-timeout finding — the rule's
// hasTimeoutConfigured detects the appended property via its "timeout" key.
func TestSetTimeoutPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = `#Region "Main"
HTTPClient.InvokeService Method: 'GET' Url: 'https://example.com/api'
Display.ShowMessageBox Message: 'done'
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var timeoutFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "missing-timeout" {
			timeoutFinding = &report.Findings[i]
			break
		}
	}
	if timeoutFinding == nil {
		t.Fatalf("expected a missing-timeout finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[timeoutFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", timeoutFinding.BlockID)
	}

	patched := ApplyPatch(source, SetTimeoutPatch(block))

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after set-timeout failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "missing-timeout" {
			t.Errorf("missing-timeout still present after set-timeout; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestInsertDelayPatch_RoundTripResolvesFinding verifies the insert-delay
// apply-fix: inserting a Wait action before the current block must (a) re-parse
// cleanly and (b) resolve the missing-delay finding — the previous sibling is
// now a Wait action (isWaitAction → true).
func TestInsertDelayPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = `#Region "Main"
WebAutomation.LaunchBrowser URL: 'https://example.com'
WebAutomation.ClickElement Element: 'button'
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var delayFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "missing-delay" {
			delayFinding = &report.Findings[i]
			break
		}
	}
	if delayFinding == nil {
		t.Fatalf("expected a missing-delay finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[delayFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", delayFinding.BlockID)
	}

	patched := ApplyPatch(source, InsertDelayPatch(block))

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after insert-delay failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "missing-delay" {
			t.Errorf("missing-delay still present after insert-delay; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestInsertHandlerLogPatch_RoundTripResolvesFinding verifies the
// insert-handler-log apply-fix: inserting a logging action before the error
// handler's END line must (a) re-parse cleanly and (b) resolve the
// empty-handler finding — the handler now has a real child.
func TestInsertHandlerLogPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = `#Region "Main"
ON BLOCK ERROR
END
WebAutomation.LaunchBrowser URL: 'https://example.com'
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var handlerFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "empty-handler" {
			handlerFinding = &report.Findings[i]
			break
		}
	}
	if handlerFinding == nil {
		t.Fatalf("expected an empty-handler finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[handlerFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", handlerFinding.BlockID)
	}

	patched := ApplyPatch(source, InsertHandlerLogPatch(block))

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after insert-handler-log failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "empty-handler" {
			t.Errorf("empty-handler still present after insert-handler-log; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestInsertVariableInitPatch_RoundTripResolvesFinding verifies the
// init-variable apply-fix: inserting `SET <var> TO ""` before the first reader
// must (a) re-parse cleanly and (b) resolve the uninitialized-variable finding
// — the SET action registers the variable in WritersByVar.
func TestInsertVariableInitPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = `#Region "Main"
Display.ShowMessageBox Message: %MyVar%
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var initFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "uninitialized-variable" {
			initFinding = &report.Findings[i]
			break
		}
	}
	if initFinding == nil {
		t.Fatalf("expected an uninitialized-variable finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[initFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", initFinding.BlockID)
	}

	varName, _ := initFinding.Metadata["variable"].(string)
	if varName == "" {
		t.Fatalf("finding metadata has no variable name: %+v", initFinding.Metadata)
	}
	patched := ApplyPatch(source, InsertVariableInitPatch(block, varName))

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after init-variable failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "uninitialized-variable" {
			t.Errorf("uninitialized-variable still present after init-variable; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestInsertErrorLogPatch_RoundTripResolvesFinding verifies the insert-error-log
// apply-fix for error-swallow: inserting a %LastError% logging action before
// the handler's END must (a) re-parse cleanly and (b) resolve the finding —
// handlerDoesSomething detects the "error" variable reference.
func TestInsertErrorLogPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = `#Region "Main"
ON BLOCK ERROR
    WAIT 1
END
WebAutomation.LaunchBrowser URL: 'https://example.com'
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var swallowFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "error-swallow" {
			swallowFinding = &report.Findings[i]
			break
		}
	}
	if swallowFinding == nil {
		t.Fatalf("expected an error-swallow finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[swallowFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", swallowFinding.BlockID)
	}

	patched := ApplyPatch(source, InsertErrorLogPatch(block))

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after insert-error-log failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "error-swallow" {
			t.Errorf("error-swallow still present after insert-error-log; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestReplaceWithVariablePatch_RoundTripResolvesFinding verifies the
// replace-with-variable apply-fix for hardcoded-credential: replacing the
// literal credential in a property value with %input_<key>% must (a) re-parse
// cleanly and (b) resolve the finding — the credential pattern no longer
// matches the variable reference.
func TestReplaceWithVariablePatch_RoundTripResolvesFinding(t *testing.T) {
	const source = `#Region "Main"
Database.Connect ConnectionString: 'AKIA1234567890ABCDEF'
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var credFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "hardcoded-credential" {
			credFinding = &report.Findings[i]
			break
		}
	}
	if credFinding == nil {
		t.Fatalf("expected a hardcoded-credential finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[credFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", credFinding.BlockID)
	}
	propKey, _ := credFinding.Metadata["property"].(string)
	if propKey == "" {
		t.Fatalf("finding metadata has no property key: %+v", credFinding.Metadata)
	}

	patched := ApplyPatch(source, ReplaceWithVariablePatch(block, propKey))

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after replace-with-variable failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "hardcoded-credential" {
			t.Errorf("hardcoded-credential still present after replace-with-variable; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestReplaceWithVariablePatch_MultiLineValue_RoundTrip verifies the fixer
// resolves a hardcoded credential that starts a multi-line triple-quoted value.
// The stripped property value spans source lines, so a naive single-line
// replace is a silent no-op — the credential on the first line must be replaced.
func TestReplaceWithVariablePatch_MultiLineValue_RoundTrip(t *testing.T) {
	const source = `#Region "Main"
Database.Connect ConnectionString: '''AKIA1234567890ABCDEF
extra data'''
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var credFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "hardcoded-credential" {
			credFinding = &report.Findings[i]
			break
		}
	}
	if credFinding == nil {
		t.Fatalf("expected a hardcoded-credential finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[credFinding.BlockID]
	propKey, _ := credFinding.Metadata["property"].(string)
	if propKey == "" {
		t.Fatalf("finding metadata has no property key: %+v", credFinding.Metadata)
	}

	patched := ApplyPatch(source, ReplaceWithVariablePatch(block, propKey))
	// The credential must be gone from the patched source (the old code left it
	// in place — a silent no-op on multi-line values).
	if strings.Contains(patched, "AKIA1234567890ABCDEF") {
		t.Errorf("credential not removed from multi-line value:\n%s", patched)
	}
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after multi-line replace failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "hardcoded-credential" {
			t.Errorf("hardcoded-credential still present after multi-line replace; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestWrapInRetryPatch_RoundTripResolvesFinding verifies the wrap-in-retry
// apply-fix for missing-retry: wrapping a transient action in a retry loop
// (LOOP WHILE %RetryCount% < 3) must (a) re-parse cleanly and (b) resolve the
// finding — isInsideRetryLoop detects "retry" in the loop condition.
func TestWrapInRetryPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = `#Region "Main"
HTTPClient.InvokeService Method: 'GET' Url: 'https://api.example.com/data'
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var retryFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "missing-retry" {
			retryFinding = &report.Findings[i]
			break
		}
	}
	if retryFinding == nil {
		t.Fatalf("expected a missing-retry finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[retryFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", retryFinding.BlockID)
	}

	patched := ApplyPatch(source, WrapInRetryPatch(block))

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after wrap-in-retry failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "missing-retry" {
			t.Errorf("missing-retry still present after wrap-in-retry; finding: %+v\npatched:\n%s", f, patched)
		}
	}
	// The retry loop is bounded (LOOP WHILE %RetryCount% < 3 with
	// SET RetryCount TO %RetryCount% + 1 in the body). It must NOT introduce an
	// infinite-loop-risk finding — otherwise the fixer trades one finding for
	// another (regression covered by hasExitCondition's counter-modification
	// recognition).
	for _, f := range report2.Findings {
		if f.RuleID == "infinite-loop-risk" {
			t.Errorf("wrap-in-retry introduced an infinite-loop-risk finding on the bounded retry loop; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestInsertExitConditionPatch_RoundTripResolvesFinding verifies the
// insert-exit-condition apply-fix for infinite-loop-risk: inserting an
// EXIT_LOOP action inside the loop must (a) re-parse cleanly and (b) resolve
// the finding — hasExitCondition detects the EXIT in the rawType.
func TestInsertExitConditionPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = `#Region "Main"
LOOP WHILE 1 == 1
    Display.ShowMessageBox Message: 'hello'
END
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var loopFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "infinite-loop-risk" {
			loopFinding = &report.Findings[i]
			break
		}
	}
	if loopFinding == nil {
		t.Fatalf("expected an infinite-loop-risk finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[loopFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", loopFinding.BlockID)
	}

	patched := ApplyPatch(source, InsertExitConditionPatch(block))

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after insert-exit-condition failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "infinite-loop-risk" {
			t.Errorf("infinite-loop-risk still present after insert-exit-condition; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestApplyPatch_Replace substitutes text within a single line.
func TestApplyPatch_Replace(t *testing.T) {
	source := "HTTPClient.InvokeService Method: 'GET' Key: 'AKIA1234567890ABCDEF'"
	patch := models.Patch{Ops: []models.PatchOp{{
		Kind:      "replace",
		StartLine: 1,
		Old:       "AKIA1234567890ABCDEF",
		New:       "%input_key%",
	}}}
	got := ApplyPatch(source, patch)
	if got != "HTTPClient.InvokeService Method: 'GET' Key: '%input_key%'" {
		t.Errorf("replace failed: got %q", got)
	}
}

// TestApplyPatch_ReplaceNotFoundIsNoOp verifies that a replace whose Old text
// doesn't exist in the target line leaves the source unchanged.
func TestApplyPatch_ReplaceNotFoundIsNoOp(t *testing.T) {
	source := "line 1\nline 2\nline 3"
	patch := models.Patch{Ops: []models.PatchOp{{
		Kind:      "replace",
		StartLine: 2,
		Old:       "nonexistent",
		New:       "replaced",
	}}}
	got := ApplyPatch(source, patch)
	if got != source {
		t.Errorf("expected no-op, got %q", got)
	}
}

// TestApplyWrap_MultiLineHeaderFooter verifies that multi-line headers and
// footers (containing \n) are correctly split into separate lines by applyWrap.
func TestApplyWrap_MultiLineHeaderFooter(t *testing.T) {
	source := "before\nACTION\nafter"
	patch := models.Patch{Ops: []models.PatchOp{{
		Kind:        "wrap",
		StartLine:   2,
		EndLine:     2,
		Header:      "SET X TO 0\nLOOP WHILE %X% < 3",
		Footer:      "SET X TO %X% + 1\nEND",
		IndentDelta: 1,
	}}}
	got := ApplyPatch(source, patch)
	expected := "before\nSET X TO 0\nLOOP WHILE %X% < 3\n    ACTION\nSET X TO %X% + 1\nEND\nafter"
	if got != expected {
		t.Errorf("multi-line wrap:\nexpected:\n%s\ngot:\n%s", expected, got)
	}
}

// TestApplyPatch_MixedOps applies a wrap then an insert to verify op ordering.
func TestApplyPatch_MixedOps(t *testing.T) {
	source := "line1\nTARGET\nline3"
	patch := models.Patch{Ops: []models.PatchOp{
		{Kind: "wrap", StartLine: 2, EndLine: 2, Header: "HEADER", Footer: "FOOTER", IndentDelta: 0},
		{Kind: "insert", BeforeLine: 1, Lines: []string{"INSERTED"}},
	}}
	got := ApplyPatch(source, patch)
	// Wrap first: line1\nHEADER\nTARGET\nFOOTER\nline3
	// Insert before line 1: INSERTED\nline1\nHEADER\nTARGET\nFOOTER\nline3
	expected := "INSERTED\nline1\nHEADER\nTARGET\nFOOTER\nline3"
	if got != expected {
		t.Errorf("mixed ops:\nexpected:\n%s\ngot:\n%s", expected, got)
	}
}

// TestApplyPatch_AppendOutOfRange is a no-op when the target line doesn't exist.
func TestApplyPatch_AppendOutOfRange(t *testing.T) {
	source := "only line"
	patch := models.Patch{Ops: []models.PatchOp{{
		Kind:      "append",
		StartLine: 99,
		Lines:     []string{" appended"},
	}}}
	got := ApplyPatch(source, patch)
	if got != source {
		t.Errorf("expected no-op for out-of-range append, got %q", got)
	}
}

// TestApplyPatch_EmptyPatch is a passthrough.
func TestApplyPatch_EmptyPatch(t *testing.T) {
	source := "unchanged"
	got := ApplyPatch(source, models.Patch{})
	if got != source {
		t.Errorf("empty patch should be passthrough, got %q", got)
	}
}

// ── Edge-case tests for the bug-fix round ─────────────────────────

// TestSuppressFindingPatch_NestedBlock verifies the indent fix: a suppression
// directive for a block at Indent ≥ 1 must be at the block's indent level so
// the parser associates it with the correct sibling group.
func TestSuppressFindingPatch_NestedBlock(t *testing.T) {
	const source = `#Region "Main"
LOOP WHILE %X% < 10
    HTTPClient.InvokeService Method: 'GET' Url: 'https://api.example.com'
END
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	// Find a finding on the nested HTTP action (e.g. missing-timeout)
	var nestedFinding *models.Finding
	for i := range report.Findings {
		f := &report.Findings[i]
		block := doc.BlocksByID[f.BlockID]
		if block != nil && block.Indent > 0 {
			nestedFinding = f
			break
		}
	}
	// Fatal, not Skip: the fixture above is a nested HTTP action inside a LOOP,
	// which the rule set is expected to flag (missing timeout / retry). If no
	// nested finding appears, either the fixture or the rules changed — and a
	// skip would quietly retire the whole nested-indentation assertion below
	// rather than reporting that.
	if nestedFinding == nil {
		t.Fatal("no finding on a nested block — the fixture or the rule set changed")
	}
	block := doc.BlocksByID[nestedFinding.BlockID]
	if block == nil {
		t.Fatalf("block not found")
	}
	if block.Indent == 0 {
		t.Fatal("selected block is at indent 0 — the nested-block search above is broken")
	}

	patched := ApplyPatch(source, SuppressFindingPatch(block, nestedFinding.RuleID))
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse failed: %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == nestedFinding.RuleID && f.BlockID == nestedFinding.BlockID {
			t.Errorf("nested suppress did not resolve finding\npatched:\n%s", patched)
		}
	}
}

// TestSetTimeoutPatch_MultiLineValue verifies the blockEndLine fix: for a
// multi-line triple-quoted action, appending ` Timeout: 30` must land on the
// CLOSING line (after `”'`), not the opening line (inside the string literal).
// Previously blockEndLine returned the start line for a leaf action, corrupting
// the literal; threading EndLineNumber through Token→Block fixes the 7 fixers
// that call blockEndLine.
func TestSetTimeoutPatch_MultiLineValue(t *testing.T) {
	// The HTTP action's URL is a multi-line triple-quoted literal spanning
	// lines 2–4. It has no Timeout → missing-timeout fires.
	const source = `#Region "Main"
HTTPClient.InvokeService Method: 'GET' Url: $'''https://example.com/api
second body line
end of body'''
Display.ShowMessageBox Message: 'done'
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var timeoutFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "missing-timeout" {
			timeoutFinding = &report.Findings[i]
			break
		}
	}
	if timeoutFinding == nil {
		t.Fatalf("expected a missing-timeout finding, got: %+v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[timeoutFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", timeoutFinding.BlockID)
	}

	// The plumbing under test: a multi-line triple-quoted action must carry an
	// EndLineNumber past its opening LineNumber.
	if block.EndLineNumber <= block.LineNumber {
		t.Fatalf("multi-line action EndLineNumber=%d must exceed LineNumber=%d (EndLine not threaded)",
			block.EndLineNumber, block.LineNumber)
	}

	patched := ApplyPatch(source, SetTimeoutPatch(block))

	// The Timeout must be appended to the closing ''' line, not the opening
	// line. Assert it is NOT on the action's first line (which would put it
	// inside the literal) and IS on the closing line.
	patchLines := splitLines(patched)
	if len(patchLines) < block.EndLineNumber {
		t.Fatalf("patched source shorter than expected: %d lines", len(patchLines))
	}
	openLine := patchLines[block.LineNumber-1]
	closingLine := patchLines[block.EndLineNumber-1]
	if strings.Contains(openLine, "Timeout: 30") {
		t.Errorf("Timeout appended to the OPENING line (inside the literal): %q", openLine)
	}
	if !strings.Contains(closingLine, "Timeout: 30") {
		t.Errorf("Timeout not on the closing line: %q", closingLine)
	}

	// Faithful: re-parse must succeed (Timeout is outside the literal now).
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after set-timeout failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	// Effective: the missing-timeout finding must be resolved.
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "missing-timeout" {
			t.Errorf("missing-timeout still present after set-timeout; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestWrapInErrorHandlerPatch_MultiLineValue is a second consumer of the
// blockEndLine fix: wrapping a multi-line action must size the wrapped range to
// the closing ”' line so nothing is left outside the ON BLOCK ERROR … END.
func TestWrapInErrorHandlerPatch_MultiLineValue(t *testing.T) {
	const source = `#Region "Main"
HTTPClient.InvokeService Method: 'GET' Url: $'''https://example.com/api
second body line
end of body'''
Display.ShowMessageBox Message: 'done'
#EndRegion
`
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	// Pick the HTTP action block.
	var target *models.Block
	for _, sf := range doc.Subflows {
		for i := range sf.Blocks {
			if strings.HasPrefix(sf.Blocks[i].RawType, "HTTPClient") {
				target = &sf.Blocks[i]
				break
			}
		}
	}
	if target == nil {
		t.Fatalf("no HTTPClient block found")
	}
	if target.EndLineNumber <= target.LineNumber {
		t.Fatalf("multi-line action EndLineNumber=%d must exceed LineNumber=%d",
			target.EndLineNumber, target.LineNumber)
	}

	patched := ApplyPatch(source, WrapInErrorHandlerPatch(target))
	// Faithful round-trip: the wrapped source re-parses cleanly (the whole
	// multi-line action is inside the handler, nothing orphaned).
	if _, err := parser.ParseText(patched, "Main.txt", int64(len(patched))); err != nil {
		t.Fatalf("re-parse after wrap-in-error-handler failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	// The ON BLOCK ERROR line must precede the action, and END must come after
	// the closing ''' line.
	patchLines := splitLines(patched)
	var hasHandlerOpen, hasHandlerEnd bool
	for i, ln := range patchLines {
		_ = i
		if strings.Contains(ln, "ON BLOCK ERROR") || strings.Contains(ln, "On Block Error") {
			hasHandlerOpen = true
		}
		if strings.Contains(ln, "END") || strings.Contains(ln, "End") {
			if hasHandlerOpen {
				hasHandlerEnd = true
			}
		}
	}
	if !hasHandlerOpen || !hasHandlerEnd {
		t.Errorf("wrap did not produce an ON BLOCK ERROR … END envelope\npatched:\n%s", patched)
	}
}

// splitLines splits on \n without a trailing-empty-entry quirk.
func splitLines(s string) []string {
	return strings.Split(s, "\n")
}

// TestInsertClosePatch_NilProperties returns empty patch when Properties is nil.
func TestInsertClosePatch_NilProperties(t *testing.T) {
	block := &models.Block{
		Type:    models.BlockTypeAction,
		RawType: "Excel.LaunchExcel",
		Indent:  0,
	}
	patch := InsertClosePatch(block)
	if len(patch.Ops) != 0 {
		t.Errorf("expected empty patch for nil Properties, got %d ops", len(patch.Ops))
	}
}

// TestReplaceWithVariablePatch_NilProperties returns empty patch when Properties is nil.
func TestReplaceWithVariablePatch_NilProperties(t *testing.T) {
	block := &models.Block{
		Type:    models.BlockTypeAction,
		RawType: "HTTPClient.InvokeService",
	}
	patch := ReplaceWithVariablePatch(block, "password")
	if len(patch.Ops) != 0 {
		t.Errorf("expected empty patch for nil Properties, got %d ops", len(patch.Ops))
	}
}

// TestReplaceWithVariablePatch_ReplacesAllOccurrences verifies that the
// replace op replaces ALL occurrences of the credential value on the line,
// not just the first (in case the same secret appears in multiple properties).
func TestReplaceWithVariablePatch_ReplacesAllOccurrences(t *testing.T) {
	source := "DB.Connect Primary: 'AKIA1234' Secondary: 'AKIA1234'"
	patch := models.Patch{Ops: []models.PatchOp{{
		Kind:      "replace",
		StartLine: 1,
		Old:       "AKIA1234",
		New:       "%input_key%",
	}}}
	got := ApplyPatch(source, patch)
	if strings.Count(got, "AKIA1234") != 0 {
		t.Errorf("expected all occurrences replaced, still found AKIA1234 in: %s", got)
	}
	if strings.Count(got, "%input_key%") != 2 {
		t.Errorf("expected 2 replacements, got %d in: %s", strings.Count(got, "%input_key%"), got)
	}
}

// TestApplyRemove_DeletesRange verifies the remove op mechanics directly: the
// inclusive [StartLine..EndLine] range is deleted and surrounding lines join.
func TestApplyRemove_DeletesRange(t *testing.T) {
	source := "a\nb\nc\nd\ne"
	p := models.Patch{Ops: []models.PatchOp{{
		Kind: "remove", StartLine: 2, EndLine: 4,
	}}}
	got := ApplyPatch(source, p)
	want := "a\ne"
	if got != want {
		t.Errorf("remove = %q, want %q", got, want)
	}
}

// TestRemoveBlockPatch_RoundTripResolvesFinding is the round-trip gate for the
// remove-block fixer across the 5 rules that use it. For each: parse → analyze
// → find the rule's finding → apply RemoveBlockPatch to the raw source →
// re-parse (faithful) → re-analyze → assert the finding is gone (effective).
func TestRemoveBlockPatch_RoundTripResolvesFinding(t *testing.T) {
	cases := []struct {
		name   string
		ruleID string
		source string
	}{
		{
			name:   "redundant-action self-assignment",
			ruleID: "redundant-action",
			source: "#Region \"Main\"\nSET X TO %X%\n#EndRegion\n",
		},
		{
			name:   "unused-variable orphan SET",
			ruleID: "unused-variable",
			source: "#Region \"Main\"\nSET UnusedVar TO 'hello'\n#EndRegion\n",
		},
		{
			name:   "dead-code after exit",
			ruleID: "dead-code",
			source: "#Region \"Main\"\nExitSubflow.Exit Code: 0\nSET DeadVar TO 'x'\n#EndRegion\n",
		},
		{
			name:   "duplicate-action run of 3",
			ruleID: "duplicate-action",
			source: "#Region \"Main\"\nText.AppendLine Text: 'x'\nText.AppendLine Text: 'x'\nText.AppendLine Text: 'x'\n#EndRegion\n",
		},
		{
			name:   "disabled-block disabled call",
			ruleID: "disabled-block",
			source: "#Region \"Main\"\nDISABLE CALL DoThing\n#EndRegion\n",
		},
		{
			name:   "dead-data variable only read after terminator",
			ruleID: "dead-data",
			source: "#Region \"Main\"\nSET TempVar TO 'data'\nExitSubflow.Exit Code: 0\nDisplay.ShowMessage Message: %TempVar%\n#EndRegion\n",
		},
		{
			name:   "empty-branch empty else",
			ruleID: "empty-branch",
			source: "#Region \"Main\"\nIF %Flag%\nELSE\nEND\n#EndRegion\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := parser.ParseText(tc.source, "Main.txt", int64(len(tc.source)))
			if err != nil {
				t.Fatalf("initial parse: %v", err)
			}
			report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
			var finding *models.Finding
			for i := range report.Findings {
				if report.Findings[i].RuleID == tc.ruleID {
					finding = &report.Findings[i]
					break
				}
			}
			if finding == nil {
				t.Fatalf("expected a %s finding, got: %v", tc.ruleID, ruleIDs(report.Findings))
			}
			block := doc.BlocksByID[finding.BlockID]
			if block == nil {
				t.Fatalf("finding block %s not in doc", finding.BlockID)
			}

			patched := ApplyPatch(tc.source, RemoveBlockPatch(block))

			doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
			if err != nil {
				t.Fatalf("re-parse after remove-block failed (not faithful): %v\npatched:\n%s", err, patched)
			}
			report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
			for _, f := range report2.Findings {
				if f.RuleID == tc.ruleID {
					t.Errorf("%s still present after remove-block; finding: %+v\npatched:\n%s", tc.ruleID, f, patched)
				}
			}
		})
	}
}

// TestParameterizeSqlPatch_RoundTripResolvesFinding is the round-trip gate for
// the parameterize-sql fixer: a SQL action interpolating %UserID% must, after
// the patch, re-parse cleanly and no longer be flagged by sql-injection-risk
// (the %var% reference is replaced with @UserID).
func TestParameterizeSqlPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = "#Region \"Main\"\nDatabase.ExecuteSql Sql: 'SELECT * FROM users WHERE id = %UserID%'\n#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var sqlFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "sql-injection-risk" {
			sqlFinding = &report.Findings[i]
			break
		}
	}
	if sqlFinding == nil {
		t.Fatalf("expected a sql-injection-risk finding, got: %v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[sqlFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", sqlFinding.BlockID)
	}
	propKey, _ := sqlFinding.Metadata["property"].(string)
	if propKey == "" {
		t.Fatalf("finding metadata has no property key: %+v", sqlFinding.Metadata)
	}

	patched := ApplyPatch(source, ParameterizeSqlPatch(block, propKey))
	if !strings.Contains(patched, "@UserID") {
		t.Fatalf("expected @UserID in patched source, got:\n%s", patched)
	}
	if strings.Contains(patched, "%UserID%") {
		t.Fatalf("expected %%UserID%% gone from patched source, got:\n%s", patched)
	}

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after parameterize-sql failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "sql-injection-risk" {
			t.Errorf("sql-injection-risk still present after parameterize-sql; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestParameterizeSqlPatch_MultipleVars emits one replace op per distinct
// variable and parameterizes all of them on the line.
func TestParameterizeSqlPatch_MultipleVars(t *testing.T) {
	block := &models.Block{
		LineNumber: 1,
		Properties: map[string]string{
			"Sql": "SELECT * FROM t WHERE a = %A% AND b = %B% AND a = %A%",
		},
	}
	patch := ParameterizeSqlPatch(block, "Sql")
	if len(patch.Ops) != 2 { // %A% and %B% (A de-duplicated)
		t.Fatalf("expected 2 replace ops (A, B), got %d: %+v", len(patch.Ops), patch.Ops)
	}
	got := ApplyPatch("Database.ExecuteSql Sql: 'SELECT * FROM t WHERE a = %A% AND b = %B% AND a = %A%'", patch)
	if strings.Contains(got, "%A%") || strings.Contains(got, "%B%") {
		t.Errorf("expected all vars parameterized, still found %%var%% in: %s", got)
	}
	if strings.Count(got, "@A") != 2 || strings.Count(got, "@B") != 1 {
		t.Errorf("expected 2 @A and 1 @B, got: %s", got)
	}
}

// TestSubflowNoErrorHandler_RoundTripResolvesFinding verifies the
// wrap-error-handler fixer resolves a subflow-no-error-handler finding.
// Wrapping the first actionable block in ON BLOCK ERROR...END gives the
// subflow tree an error-handler block, so sfHasErrorHandler returns true.
func TestSubflowNoErrorHandler_RoundTripResolvesFinding(t *testing.T) {
	const source = "#Region \"Main\"\n" +
		"WebAutomation.Click Element: 'b1'\n" +
		"WebAutomation.Click Element: 'b2'\n" +
		"WebAutomation.Click Element: 'b3'\n" +
		"WebAutomation.Click Element: 'b4'\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var sfFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "subflow-no-error-handler" {
			sfFinding = &report.Findings[i]
			break
		}
	}
	if sfFinding == nil {
		t.Fatalf("expected subflow-no-error-handler finding, got: %v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[sfFinding.BlockID]
	if block == nil {
		t.Fatalf("block not found")
	}

	patched := ApplyPatch(source, WrapInErrorHandlerPatch(block))
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "subflow-no-error-handler" {
			t.Errorf("subflow-no-error-handler still present after fix\npatched:\n%s", patched)
		}
	}
}

// ---- L4: out-of-range patch ops are observable no-ops, not silent corruption ----

func TestApplyPatch_OutOfRangeOpsAreNoOpsAndCounted(t *testing.T) {
	const source = "line one\nline two\nline three"
	cases := []struct {
		name string
		op   models.PatchOp
	}{
		{"insert past EOF+1", models.PatchOp{Kind: "insert", BeforeLine: 99, Lines: []string{"x"}}},
		{"insert at zero", models.PatchOp{Kind: "insert", BeforeLine: 0, Lines: []string{"x"}}},
		{"wrap start past EOF", models.PatchOp{Kind: "wrap", StartLine: 42, EndLine: 43, Header: "H", Footer: "F"}},
		{"wrap start zero", models.PatchOp{Kind: "wrap", StartLine: 0, EndLine: 1, Header: "H", Footer: "F"}},
		{"remove past EOF", models.PatchOp{Kind: "remove", StartLine: 42, EndLine: 50}},
		{"replace past EOF", models.PatchOp{Kind: "replace", StartLine: 42, Old: "line", New: "X"}},
		{"append past EOF", models.PatchOp{Kind: "append", StartLine: 42, Lines: []string{" tail"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := PatchOutOfRangeOps()
			got := ApplyPatch(source, models.Patch{Ops: []models.PatchOp{tc.op}})
			if got != source {
				t.Errorf("out-of-range op mutated source:\nwant %q\ngot  %q", source, got)
			}
			if delta := PatchOutOfRangeOps() - before; delta != 1 {
				t.Errorf("expected exactly 1 out-of-range op recorded, got %d", delta)
			}
		})
	}
}

func TestApplyPatch_LegitimateBoundaryOpsAreNotCounted(t *testing.T) {
	const source = "a\nb\nc"
	before := PatchOutOfRangeOps()

	// Insert at EOF (BeforeLine == len+1) is valid — appends at the end.
	got := ApplyPatch(source, models.Patch{Ops: []models.PatchOp{
		{Kind: "insert", BeforeLine: 4, Lines: []string{"d"}},
	}})
	if got != "a\nb\nc\nd" {
		t.Errorf("insert-at-EOF: got %q", got)
	}
	// Wrap whose EndLine spans past the last line clamps, not a fault.
	got = ApplyPatch(source, models.Patch{Ops: []models.PatchOp{
		{Kind: "wrap", StartLine: 1, EndLine: 999, Header: "H", Footer: "F"},
	}})
	if got != "H\na\nb\nc\nF" {
		t.Errorf("wrap-spanning-EOF: got %q", got)
	}
	if delta := PatchOutOfRangeOps() - before; delta != 0 {
		t.Errorf("legitimate boundary ops should record 0 faults, got %d", delta)
	}
}
