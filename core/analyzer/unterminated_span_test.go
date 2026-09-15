package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// hiddenPayload is the line an evasion is trying to keep out of the report. It
// trips more than one rule on its own, so "no findings" can only mean the
// analyzer never saw it.
const hiddenPayload = "System.RunApplication ApplicationPath: $'''powershell.exe''' " +
	"CommandLineArguments: $'''-enc SQBFAFgAIAAoAE4AZQB3AC0ATwBiAGoA'''\n"

// TestUnterminatedSpan_CannotHideTheRestOfTheFile is the regression gate for a
// full analyzer evasion.
//
// Tokenize consumed every line after an opening /# (or an odd ”' on an ACTION
// line) into a strings.Builder and emitted nothing until the closing marker.
// When the marker never arrived, the builder was dropped at `return tokens` --
// no token, no block, and no ParseError. So a flow beginning with one unclosed
// comment marker parsed to an EMPTY document and the report came back clean,
// with the payload above never reaching a single rule. Both spans are reachable
// from /api/analysis/analyze-raw, which accepts arbitrary text.
//
// The assertion is deliberately about the VERDICT, not about tokens: emitting a
// ParseError that no rule surfaces would close nothing. parse-error is an Error
// rule, and engine.go's document-level fallback emits it even when no block can
// anchor it -- this test is what ties those two together.
func TestUnterminatedSpan_CannotHideTheRestOfTheFile(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "unclosed block comment",
			src:  "#Region \"Main\"\n/# maintenance note\n" + hiddenPayload + "#EndRegion\n",
		},
		{
			name: "unclosed triple-quoted string",
			src:  "#Region \"Main\"\nDisplay.ShowMessageBox Message: $'''oops\n" + hiddenPayload + "#EndRegion\n",
		},
		{
			// No #Region at all: the swallow happens before any subflow exists,
			// so the parse-error rule has neither a block nor a subflow to
			// anchor to. This is the case the per-block rule cannot reach.
			name: "unclosed block comment with no subflow",
			src:  "/# note\n" + hiddenPayload,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := parser.ParseText(tc.src, "Main.txt", int64(len(tc.src)))
			if err != nil {
				t.Fatalf("ParseText: %v", err)
			}

			if len(doc.ParseErrors) == 0 {
				t.Fatalf("the file was consumed with no diagnostic at all -- "+
					"an analysis of this input reports clean.\nsource:\n%s", tc.src)
			}

			// Severity must be "error": CountParseErrors ignores warnings, so a
			// warning here would leave the CI gate and the fix loop's
			// regression guard blind to a file that hid its own contents.
			if n := CountParseErrors(doc); n == 0 {
				t.Errorf("%d parse error(s) recorded, but none at error severity -- "+
					"CountParseErrors counts only errors, so the gate stays green", len(doc.ParseErrors))
			}

			report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)

			var errorFindings int
			for _, f := range report.Findings {
				if f.Severity == models.SeverityError {
					errorFindings++
				}
			}
			if errorFindings == 0 {
				t.Errorf("analysis of a file that hid its own contents produced no Error finding "+
					"(%d finding(s) total) -- the report is indistinguishable from a clean flow",
					len(report.Findings))
			}

			var sawParseError bool
			for _, f := range report.Findings {
				if f.RuleID == "parse-error" && strings.Contains(f.Description, "unterminated") {
					sawParseError = true
				}
			}
			if !sawParseError {
				t.Errorf("no parse-error finding names the unterminated span; the user is told "+
					"something is wrong but not what.\nfindings: %+v", report.Findings)
			}
		})
	}
}

// TestUnterminatedSpan_ContentSurvivesIntoTheTree is the other half: the flush
// must preserve the consumed text, not merely report an error. Dropping the text
// while recording an error would still lose the source for the editor, the
// serializer and every block-level fixer.
func TestUnterminatedSpan_ContentSurvivesIntoTheTree(t *testing.T) {
	src := "#Region \"Main\"\n/# note\n" + hiddenPayload + "#EndRegion\n"
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	var total int
	for i := range doc.Subflows {
		total += len(doc.Subflows[i].Blocks)
	}
	if total == 0 {
		t.Fatalf("the consumed lines produced no block at all -- the text is still lost:\n%s", src)
	}
}

// TestClosedSpans_AreUnaffected guards the flush from being drawn too wide: a
// properly closed comment or literal must tokenize exactly as before, with no
// spurious parse error.
func TestClosedSpans_AreUnaffected(t *testing.T) {
	cases := map[string]string{
		"closed block comment": "#Region \"Main\"\n/# a note\nstill the note #/\n" +
			"HTTPClient.InvokeUrl Method: GET Url: $'''https://api.example.com/x'''\n#EndRegion\n",
		"closed multi-line literal": "#Region \"Main\"\n" +
			"Display.ShowMessageBox Message: $'''line one\nline two'''\n#EndRegion\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
			if err != nil {
				t.Fatalf("ParseText: %v", err)
			}
			for _, pe := range doc.ParseErrors {
				if strings.Contains(pe.Message, "unterminated") {
					t.Errorf("a correctly closed span was reported unterminated: %+v", pe)
				}
			}
		})
	}
}
