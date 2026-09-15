package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// retryTargetFlow is a single send-email action — one of missing-retry's own
// targets (rule_missing_retry.go's transient-operation prefixes).
const retryTargetFlow = "#Region \"Main\"\n" +
	"Outlook.SendEmailThroughOutlook Account: 'ops@corp' To: %Customer% Subject: 'Invoice' Body: %InvoiceHtml%\n" +
	"#EndRegion\n"

func countOccurrences(s, sub string) int { return strings.Count(s, sub) }

// TestWrapInRetry_DoesNotRepeatTheActionUnconditionally is the regression gate
// for the most expensive fixer bug in the analyzer.
//
// The emitted structure was:
//
//	SET RetryCount TO 0
//	LOOP WHILE %RetryCount% < 3
//	    <action>
//	    SET RetryCount TO %RetryCount% + 1
//	END
//
// There is no error check and no success exit, so this is not a retry — it is an
// unconditional 3× repeat. Applied to missing-retry's own targets that means
// three invoices to the customer, three Database.ExecuteSqlStatement inserts, or
// three paid API calls. The round-trip gate passed the whole time, because it
// only asserts the finding disappeared.
func TestWrapInRetry_DoesNotRepeatTheActionUnconditionally(t *testing.T) {
	src := retryTargetFlow
	n, err := ApplyFixesToSource(&src, "Main.txt", FixOptions{
		RuleFilter: map[string]bool{"missing-retry": true},
		Limit:      20,
	})
	if err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}
	if n == 0 {
		t.Fatalf("fixture has rotted: missing-retry produced no fix.\nsource:\n%s", src)
	}

	// The action must still execute exactly once per pass.
	if got := countOccurrences(src, "Outlook.SendEmailThroughOutlook"); got != 1 {
		t.Errorf("the action appears %d times after the fix, want 1:\n%s", got, src)
	}
	if strings.Contains(src, "LOOP WHILE %RetryCount%") {
		t.Errorf("the fix still wraps the action in an unconditional counting loop — "+
			"that repeats a send/insert/charge three times:\n%s", src)
	}
	// PAD's own retry construct is conditional on failure, which is what a retry
	// means. The parser already folds it into _retryCount.
	if !strings.Contains(strings.ToUpper(src), "ON ERROR REPEAT") {
		t.Errorf("expected PAD's native ON ERROR REPEAT retry, got:\n%s", src)
	}
}

// TestWrapInRetry_ResultReParsesWithRetryConfigured closes the loop: the emitted
// line must be understood by the parser, not merely look right.
func TestWrapInRetry_ResultReParsesWithRetryConfigured(t *testing.T) {
	src := retryTargetFlow
	if _, err := ApplyFixesToSource(&src, "Main.txt", FixOptions{
		RuleFilter: map[string]bool{"missing-retry": true},
		Limit:      20,
	}); err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}

	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("re-parse after fix: %v\n%s", err, src)
	}
	var found bool
	var walk func(bs []models.Block)
	walk = func(bs []models.Block) {
		for i := range bs {
			if strings.HasPrefix(bs[i].RawType, "Outlook.") && bs[i].Properties["_retryCount"] != "" {
				found = true
			}
			walk(bs[i].Children)
		}
	}
	for i := range doc.Subflows {
		walk(doc.Subflows[i].Blocks)
	}
	if !found {
		t.Errorf("the fixed action carries no _retryCount after re-parsing — the emitted retry "+
			"line is not the shape the parser folds:\n%s", src)
	}
}

// TestNativeRetry_SuppressesMissingRetry pins the false positive that made the
// fixer necessary in the first place.
//
// PAD's inline `ON ERROR REPEAT N TIMES WAIT M` is parsed into _retryCount /
// _retryWait (parser.go) and was then read by NO rule — so an action with a
// retry already configured natively still drew missing-retry, and `bakicli fix`
// would "fix" it by adding a counting loop on top of a working retry.
//
// Scope note: this deliberately does NOT assert that unhandled-error stands
// down. A retry re-runs the action; when the last attempt fails the error still
// propagates, so a retry is not error handling and unhandled-error is right to
// keep firing. Guarding that rule too was an over-reach on my part, caught by
// two service tests that expect an ON BLOCK ERROR handler to still be added.
func TestNativeRetry_SuppressesMissingRetry(t *testing.T) {
	src := "#Region \"Main\"\n" +
		"HTTPClient.InvokeWebService Url: $'''https://api.corp/orders''' Method: GET Timeout: 60 => Response\n" +
		"ON ERROR REPEAT 3 TIMES WAIT 5\n" +
		"#EndRegion\n"

	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)

	if got := findingsByRule(report, "missing-retry"); len(got) != 0 {
		t.Errorf("missing-retry fired on an action with a native ON ERROR REPEAT retry configured (%d finding(s))",
			len(got))
	}
}
