package scrubber

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestScrubSourceText_PADSecretSyntax pins the S1 contract: PAD's SET-TO and
// colon-delimited property syntax must be masked in raw source text with the
// same rules as the document scrubber. Raw disk bytes used to reach the model
// verbatim because no key=value regex matches `SET Password TO $”'secret”'`.
func TestScrubSourceText_PADSecretSyntax(t *testing.T) {
	src := `FUNCTION Main
    SET Username TO $'''admin'''
    SET Password TO $'''super-secret-password'''
    WebAutomation.FillTextByText Text: $'''password''' FillWith: $'''hunter2-secret'''
    WebAutomation.PopulateTextField BrowserInstance: %Browser% Control: appmask['field'] Text: $'''MyP@ssw0rd!2024'''
    Variables.SetVariable NewValue: %Password% Name: HardcodedCred
    WebAutomation.NavigateTo Url: $'''https://example.com/login?user=%Username%''' BrowserInstance: %Browser%
END`

	got := ScrubSourceText(src)

	if strings.Contains(got, "super-secret-password") {
		t.Errorf("SET Password TO literal leaked: %q", got)
	}
	if strings.Contains(got, "hunter2-secret") {
		t.Errorf("sensitive action field (FillWith) leaked: %q", got)
	}
	if strings.Contains(got, "MyP@ssw0rd!2024") {
		t.Errorf("PopulateTextField.Text leaked: %q", got)
	}
	// The username is not a credential-named field: it must survive for the
	// model to reason about the flow.
	if !strings.Contains(got, "admin") {
		t.Errorf("non-sensitive literal wrongly masked: %q", got)
	}
	if !strings.Contains(got, "https://example.com/login") {
		t.Errorf("URL wrongly masked: %q", got)
	}
	// Variable references and structural text survive.
	if !strings.Contains(got, "SET Password TO") || !strings.Contains(got, "%Browser%") {
		t.Errorf("structure/var refs corrupted: %q", got)
	}
}

// TestScrubSourceText_PureVarRefsNotMasked: `SET NewPwd TO %Password%` carries
// no literal secret (the value is a variable reference) — masking it would
// only destroy information the model needs.
func TestScrubSourceText_PureVarRefsNotMasked(t *testing.T) {
	src := "FUNCTION Main\n    SET NewPassword TO %Password%\n    SET Short TO 'ab'\nEND"
	got := ScrubSourceText(src)
	if !strings.Contains(got, "%Password%") {
		t.Errorf("pure var reference masked: %q", got)
	}
}

// TestScrubSourceText_UnparseableFallsBackToRegex: garbage input must return
// the regex-pass result, not an error state.
func TestScrubSourceText_UnparseableFallsBackToRegex(t *testing.T) {
	got := ScrubSourceText("not a flow at all\npassword=hunter2secret42")
	if strings.Contains(got, "hunter2secret42") {
		t.Errorf("regex fallback failed to mask key=value secret: %q", got)
	}
}

// TestScrubDocument_SETSyntax masks the same class of secret in the DOCUMENT
// path: the parser injects _var/_value for SET lines, and the field-name pass
// can't see the synthetic keys — `SET Password TO $”'secret”'` used to
// survive ScrubDocument intact.
func TestScrubDocument_SETSyntax(t *testing.T) {
	doc, err := parser.ParseText("FUNCTION Main\n    SET Password TO $'''super-secret-password'''\nEND", "t", 60)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := ScrubDocument(doc)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	blk := &got.Subflows[0].Blocks[0]
	if v := blk.Properties["_value"]; v != "[REDACTED]" {
		t.Errorf("_value = %q, want [REDACTED]", v)
	}
	if v := blk.Properties["_var"]; v != "Password" {
		t.Errorf("_var = %q, want Password (the NAME is not a secret)", v)
	}
}

// TestScrubSourceText_RealSample drives the shipped complex_flow sample (which
// contains exactly the MySecretPassword123! fixture) end-to-end.
func TestScrubSourceText_RealSample(t *testing.T) {
	src := "FUNCTION Main\n    SET Password TO $'''MySecretPassword123!'''\n    Variables.SetVariable NewValue: %Password% Name: HardcodedCred\nEND"
	got := ScrubSourceText(src)
	if strings.Contains(got, "MySecretPassword123") {
		t.Errorf("sample secret leaked: %q", got)
	}
}

// TestCollectSensitiveValues_DedupesAndSkips guards the collector's own
// guards: duplicates collapse, short values and var refs are skipped.
func TestCollectSensitiveValues_DedupesAndSkips(t *testing.T) {
	doc := &models.FlowDocument{Subflows: []models.Subflow{{
		Blocks: []models.Block{
			{RawType: "Generic", Properties: map[string]string{"password": "dup-secret", "token": "%Var%"}},
			{RawType: "Generic", Properties: map[string]string{"pwd": "dup-secret", "apikey": "ab"}},
			{Properties: map[string]string{"_var": "Password", "_value": "set-secret"}},
		},
	}}}
	vals := collectSensitiveValues(doc)
	joined := strings.Join(vals, "\x00")
	if strings.Count(joined, "dup-secret") != 1 {
		t.Errorf("duplicate value collected twice: %v", vals)
	}
	if strings.Contains(joined, "%Var%") || strings.Contains(joined, "ab") {
		t.Errorf("var-ref/short value collected: %v", vals)
	}
	if !strings.Contains(joined, "set-secret") {
		t.Errorf("SET-TO value missing: %v", vals)
	}
}
