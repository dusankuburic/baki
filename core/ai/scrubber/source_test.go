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

// parityOpaqueA and parityOpaqueB pass the entropy check and match no regex class.
const (
	parityOpaqueA = "Mx4bVq7nRt2kZw9yLp3dJc8sHf5gBa1eQu6iOy0r"
	parityOpaqueB = "Zq8vN2pL5xR9tW3kY7mB4cF6hJ1dG0sA2eU5iO8p"
	parityOpaqueC = "kq7zx4vb2nm8kl5pr3tw9yj6hd1gf0sa2eu5io8c"
)

// paritySrc holds one literal per rule of the document scrubber — action field, credential key, SET target,
// header literal, comment word, entropy fallback on a SET value and on an unlisted property — next to the
// literals that no rule may touch. Every literal is unique in the text, so a replacement can be attributed.
const paritySrc = "#Region \"Main\"\n" +
	"    # Runbook token " + parityOpaqueB + " rotates quarterly\n" +
	"    SET Username TO $'''svc-reporting'''\n" +
	"    SET Password TO $'''super-secret-pw'''\n" +
	"    SET ServiceKey TO $'''" + parityOpaqueA + "'''\n" +
	"    WebAutomation.FillTextByText Text: $'''login-field''' FillWith: $'''hunter2-fill'''\n" +
	"    Email.ConnectToSMTP Server: $'''smtp.example.com''' Password: $'''smtp-secret-pw''' Port: 587\n" +
	"    Custom.Vault Note: $'''" + parityOpaqueC + "'''\n" +
	"    IF %DbPassword% = $'''Pr0d!Pass''' THEN\n" +
	"        Display.ShowMessageDialog Title: $'''Greeting''' Message: $'''Welcome aboard'''\n" +
	"    END\n" +
	"    IF %Status% = $'''Active''' THEN\n" +
	"        WAIT 1\n" +
	"    END\n" +
	"    SWITCH %Password%\n" +
	"        CASE = $'''CaseSecret1'''\n" +
	"            WAIT 1\n" +
	"    END\n" +
	"    WebAutomation.NavigateTo Url: $'''https://example.com/login''' BrowserInstance: %Browser%\n" +
	"#EndRegion\n"

// parityLiterals are the literal contents of paritySrc, each with the verdict both scrubbers must share.
var parityLiterals = map[string]bool{
	parityOpaqueB:               true, // comment word, entropy
	"svc-reporting":             false,
	"super-secret-pw":           true, // SET of a credential
	parityOpaqueA:               true, // SET value, entropy fallback on a plain variable
	"login-field":               false,
	"hunter2-fill":              true, // sensitiveActions field
	"smtp.example.com":          false,
	"smtp-secret-pw":            true, // credential key
	parityOpaqueC:               true, // unlisted property, entropy fallback
	"Pr0d!Pass":                 true, // IF header on a credential variable
	"Greeting":                  false,
	"Welcome aboard":            false,
	"Active":                    false,
	"CaseSecret1":               true, // CASE under a credential SWITCH
	"https://example.com/login": false,
}

// docHolds reports whether any Name or property value of the document contains s.
func docHolds(doc *models.FlowDocument, s string) bool {
	var walk func(blocks []models.Block) bool
	walk = func(blocks []models.Block) bool {
		for i := range blocks {
			if strings.Contains(blocks[i].Name, s) {
				return true
			}
			for _, v := range blocks[i].Properties {
				if strings.Contains(v, s) {
					return true
				}
			}
			if walk(blocks[i].Children) {
				return true
			}
		}
		return false
	}
	for _, sf := range doc.Subflows {
		if walk(sf.Blocks) {
			return true
		}
	}
	return false
}

// TestScrubParity_DocumentAndSourceText runs one fixture through ScrubDocument and ScrubSourceText: every
// literal masked by one is masked by the other, and both agree with the verdict the fixture expects — the
// high-entropy value assigned to a variable without a credential name included.
func TestScrubParity_DocumentAndSourceText(t *testing.T) {
	doc, err := parser.ParseText(paritySrc, "parity", int64(len(paritySrc)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for lit := range parityLiterals {
		if !docHolds(doc, lit) || !strings.Contains(paritySrc, lit) {
			t.Fatalf("fixture literal %q is not in the parsed document or the source", lit)
		}
	}
	scrubbedDoc, err := ScrubDocument(doc)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	scrubbedSrc := ScrubSourceText(paritySrc)

	for lit, want := range parityLiterals {
		byDoc := !docHolds(scrubbedDoc, lit)
		bySrc := !strings.Contains(scrubbedSrc, lit)
		if byDoc != bySrc {
			t.Errorf("%q: ScrubDocument masked=%v, ScrubSourceText masked=%v", lit, byDoc, bySrc)
		}
		if byDoc != want {
			t.Errorf("%q: masked=%v, want %v", lit, byDoc, want)
		}
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
