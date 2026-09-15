package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// brokenFlow produces one parse error per line at five bytes of input each.
func brokenFlow(lines int) string {
	return "#Region \"M\"\n" + strings.Repeat("ELSE\n", lines)
}

func analyzeBroken(t *testing.T, lines int) (*models.FlowDocument, *models.AnalysisReport) {
	t.Helper()
	src := brokenFlow(lines)
	doc, err := parser.ParseText(src, "M.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	return doc, RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
}

// TestParseErrorFindings_AreCapped is the resource gate for the analysis
// endpoints.
//
// Parse errors became findings 1:1 with no cap, and a parse error costs as
// little as five bytes of input, so finding count scaled with file size:
// measured 2,048 findings from 10 KB, 20,480 from 100 KB and 209,715 from 1 MB
// (894 MB allocated) — against an endpoint that accepts 2 MB. Every finding is
// then serialised into the response, which is where the ~141x
// request-to-response amplification and the multi-hundred-megabyte bodies came
// from, and is retained afterwards by the report cache.
func TestParseErrorFindings_AreCapped(t *testing.T) {
	for _, lines := range []int{500, 5_000, 50_000} {
		doc, rep := analyzeBroken(t, lines)
		if len(doc.ParseErrors) < lines/2 {
			t.Fatalf("fixture rot: %d lines produced only %d parse errors", lines, len(doc.ParseErrors))
		}
		var pe int
		for _, f := range rep.Findings {
			if f.RuleID == "parse-error" {
				pe++
			}
		}
		// maxParseErrorFindings individual findings, plus one summary.
		if pe > maxParseErrorFindings+1 {
			t.Errorf("%d input lines produced %d parse-error findings (cap is %d+1) — the "+
				"response scales with the attacker's input", lines, pe, maxParseErrorFindings)
		}
	}
}

// TestParseErrorFindings_TruncationIsReported is the half that keeps the cap
// honest. A report that silently omits findings is indistinguishable from a
// cleaner file, which is the exact failure this codebase has had to correct more
// than once — so the omission must appear IN the report, with the real total.
func TestParseErrorFindings_TruncationIsReported(t *testing.T) {
	doc, rep := analyzeBroken(t, 5_000)

	var summary *models.Finding
	for i := range rep.Findings {
		if strings.Contains(rep.Findings[i].Title, "truncated") {
			summary = &rep.Findings[i]
		}
	}
	if summary == nil {
		t.Fatal("findings were dropped with no summary — the report looks cleaner than the file is")
	}
	total, _ := summary.Metadata["totalParseErrors"].(int)
	if total != len(doc.ParseErrors) {
		t.Errorf("summary reports %d total parse errors, the document has %d", total, len(doc.ParseErrors))
	}
	omitted, _ := summary.Metadata["omitted"].(int)
	if omitted != len(doc.ParseErrors)-maxParseErrorFindings {
		t.Errorf("summary reports %d omitted, expected %d", omitted, len(doc.ParseErrors)-maxParseErrorFindings)
	}
}

// TestParseErrorFindings_CapDoesNotWeakenTheGate is the assertion that matters
// most operationally, and it targets the sharp case: the Error-severity problems
// appear AFTER the cap position, buried behind thousands of warnings.
//
// Truncating the LIST must never lower the report's severity ceiling, or a file
// whose real errors happen to sort late would start passing CI. The summary
// finding therefore carries the highest severity among the errors it omitted.
//
// (The bulk filler here is `ELSE` outside an IF, which is a WARNING-severity
// parse error — an earlier version of this test assumed it was an Error and
// failed for that reason, not because the cap had broken anything.)
func TestParseErrorFindings_CapDoesNotWeakenTheGate(t *testing.T) {
	// Thousands of warning-severity errors, then an unterminated block comment,
	// which the tokenizer reports at ERROR severity.
	src := "#Region \"M\"\n" + strings.Repeat("ELSE\n", 5_000) + "/# never closed\n"
	doc, err := parser.ParseText(src, "M.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}

	var srcErrors int
	for _, pe := range doc.ParseErrors {
		if pe.Severity == "error" {
			srcErrors++
		}
	}
	if srcErrors == 0 {
		t.Fatal("fixture rot: this input no longer produces an Error-severity parse error, " +
			"so the test cannot detect a weakened gate")
	}

	rep := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var errs int
	for _, f := range rep.Findings {
		if f.Severity == models.SeverityError {
			errs++
		}
	}
	if errs == 0 {
		t.Errorf("the document has %d Error-severity parse errors, but after truncation the "+
			"report has none — a broken file would now pass the gate", srcErrors)
	}
}

// TestParseErrorFindings_SmallFilesAreUnchanged keeps the cap invisible for
// ordinary input: a file with a handful of errors must still list every one,
// with no summary noise.
func TestParseErrorFindings_SmallFilesAreUnchanged(t *testing.T) {
	doc, rep := analyzeBroken(t, 3)
	var pe int
	for _, f := range rep.Findings {
		if f.RuleID == "parse-error" {
			pe++
			if strings.Contains(f.Title, "truncated") {
				t.Error("a small file got a truncation summary it does not need")
			}
		}
	}
	if pe != len(doc.ParseErrors) {
		t.Errorf("listed %d of %d parse errors for a small file — every one should be shown",
			pe, len(doc.ParseErrors))
	}
}

// TestBuildParseErrorFindings_SummaryCarriesTheWorstOmittedSeverity tests the
// helper DIRECTLY, because the end-to-end path cannot see this.
//
// models.DefaultSettings() configures a severity override for parse-error, and
// the engine applies overrides to every finding a rule returns — so in the
// default configuration all parse-error findings come out Error regardless of
// the individual error's severity. That is what preserves the gate end to end,
// and it means TestParseErrorFindings_CapDoesNotWeakenTheGate would pass even if
// this logic were wrong.
//
// The ceiling still matters wherever no override is configured (RunAnalysis with
// nil settings, or a deployment that sets parse-error to warning): truncating
// the list must not drop the severity of what was omitted.
func TestBuildParseErrorFindings_SummaryCarriesTheWorstOmittedSeverity(t *testing.T) {
	errs := make([]models.ParseError, 0, maxParseErrorFindings+5)
	for i := 0; i < maxParseErrorFindings; i++ {
		errs = append(errs, models.ParseError{Line: i + 1, Severity: "info", Message: "noise"})
	}
	// The only ERROR sits beyond the cap, so it appears in no individual finding.
	errs = append(errs,
		models.ParseError{Line: 999, Severity: "warning", Message: "late warning"},
		models.ParseError{Line: 1000, Severity: "error", Message: "late error"},
	)

	out := buildParseErrorFindings(errs, "b1", "sf1")

	var summary *models.Finding
	for i := range out {
		if strings.Contains(out[i].Title, "truncated") {
			summary = &out[i]
		}
	}
	if summary == nil {
		t.Fatal("no truncation summary was emitted")
	}
	if summary.Severity != models.SeverityError {
		t.Errorf("summary severity is %q, want error — the only Error-severity parse error was "+
			"omitted by the cap, so without this the report's severity ceiling silently drops",
			summary.Severity)
	}

	// And none of the individually-listed findings is an Error, proving the
	// summary is genuinely the only carrier here.
	for _, f := range out[:len(out)-1] {
		if f.Severity == models.SeverityError {
			t.Fatal("fixture rot: an Error appeared before the cap, so this test proves nothing")
		}
	}
}
