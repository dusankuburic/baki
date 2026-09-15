package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestHardcodedCredentialRule_PropertyKey covers the key-aware check: a
// credential-typed property — by its name (Password, ApiKey, ClientSecret, …)
// or by the action's contract (a Cryptography Key) — holding a literal is
// flagged, once, with the `property` metadata the replace-with-variable fixer
// keys on. Variable references, placeholders, short values, output variables
// and non-credential keys are not.
func TestHardcodedCredentialRule_PropertyKey(t *testing.T) {
	rule := &HardcodedCredentialRule{}
	const keyTitle = "Credential property set to literal value"
	const patternTitle = "Hardcoded credential detected"

	cases := []struct {
		name      string
		rawType   string
		blockType models.BlockType
		props     map[string]string
		wantTitle string // "" = no finding at all
		wantProp  string // expected Metadata["property"]
	}{
		{
			name: "Password literal on SMTP send", rawType: "Email.SendEmailSMTP", blockType: models.BlockTypeAction,
			props:     map[string]string{"Server": "smtp.corp.local", "Password": "Wint3r2024", "To": "ops@corp"},
			wantTitle: keyTitle, wantProp: "Password",
		},
		{
			name: "Password variable reference is not a literal", rawType: "Email.SendEmailSMTP", blockType: models.BlockTypeAction,
			props: map[string]string{"Server": "smtp.corp.local", "Password": "%Cred%"},
		},
		{
			name: "ApiKey literal", rawType: "HTTPClient.InvokeUrl", blockType: models.BlockTypeAction,
			props:     map[string]string{"Url": "https://api.example.com", "ApiKey": "sk-live-0123456789"},
			wantTitle: keyTitle, wantProp: "ApiKey",
		},
		{
			name: "ClientSecret literal", rawType: "HTTPClient.InvokeUrl", blockType: models.BlockTypeAction,
			props:     map[string]string{"ClientSecret": "s3cr3t-value"},
			wantTitle: keyTitle, wantProp: "ClientSecret",
		},
		{
			name: "Cryptography Key by action contract", rawType: "Cryptography.EncryptTextWithAES", blockType: models.BlockTypeAction,
			props:     map[string]string{"TextToEncrypt": "%Plain%", "Key": "correct-horse-battery"},
			wantTitle: keyTitle, wantProp: "Key",
		},
		{
			name: "Key on an unrelated action is not a credential", rawType: "Dictionary.GetValue", blockType: models.BlockTypeAction,
			props: map[string]string{"Key": "customerName"},
		},
		{
			name: "masked placeholder", rawType: "Email.SendEmailSMTP", blockType: models.BlockTypeAction,
			props: map[string]string{"Password": "********"},
		},
		{
			name: "boolean under a Password-ish key", rawType: "Excel.LaunchExcel", blockType: models.BlockTypeAction,
			props: map[string]string{"UsePassword": "True"},
		},
		{
			name: "too short to be a secret", rawType: "Email.SendEmailSMTP", blockType: models.BlockTypeAction,
			props: map[string]string{"Password": "abc"},
		},
		{
			name: "output variable named Password is a target, not a value", rawType: "Cryptography.DecryptTextWithAES", blockType: models.BlockTypeAction,
			props: map[string]string{"_output": "Password", "TextToDecrypt": "%Cipher%"},
		},
		{
			name: "loop block is not an action", rawType: "LOOP", blockType: models.BlockTypeLoop,
			props: map[string]string{"Password": "Wint3r2024"},
		},
		{
			name: "unquoted password inside a connection string", rawType: "Database.Connect", blockType: models.BlockTypeAction,
			props:     map[string]string{"ConnectionString": "Server=db1;Database=hr;User Id=svc;Password=Wint3r2024;"},
			wantTitle: patternTitle, wantProp: "ConnectionString",
		},
		{
			name: "connection string without a password", rawType: "Database.Connect", blockType: models.BlockTypeAction,
			props: map[string]string{"ConnectionString": "Server=db1;Database=hr;Integrated Security=true"},
		},
		{
			name: "connection string password from a variable", rawType: "Database.OpenSQLConnection", blockType: models.BlockTypeAction,
			props: map[string]string{"ConnectionString": "Server=db1;Password=%DbPassword%;"},
		},
		{
			name: "pattern and key both match yields one finding", rawType: "Email.SendEmailSMTP", blockType: models.BlockTypeAction,
			props:     map[string]string{"Password": "password='mysecret123'"},
			wantTitle: patternTitle, wantProp: "Password",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := makeBlock("b1", "step", tc.blockType, tc.rawType, 0)
			b.Properties = tc.props
			b.SubflowID = "sf1"
			flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
			got := rule.Check(b, buildContext(flow, nil))
			if tc.wantTitle == "" {
				if len(got) != 0 {
					t.Fatalf("expected no finding, got %d: %+v", len(got), got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly 1 finding, got %d: %+v", len(got), got)
			}
			if got[0].Title != tc.wantTitle {
				t.Errorf("title = %q, want %q", got[0].Title, tc.wantTitle)
			}
			if p, _ := got[0].Metadata["property"].(string); p != tc.wantProp {
				t.Errorf("metadata.property = %q, want %q (the replace-with-variable fixer keys on it)", p, tc.wantProp)
			}
		})
	}
}

// TestHardcodedCredentialRule_PasswordPropertyRoundTrip drives the real parser
// and the real fixer: the finding on `Password: 'Wint3r2024'` carries the
// property the replace-with-variable fixer needs, and applying that fix
// resolves the finding on re-analysis.
func TestHardcodedCredentialRule_PasswordPropertyRoundTrip(t *testing.T) {
	const src = "#Region \"Main\"\n" +
		"Email.SendEmailSMTP Server: 'smtp.corp.local' Password: 'Wint3r2024' To: 'ops@corp'\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	rep := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)

	var target *models.Finding
	for i := range rep.Findings {
		if rep.Findings[i].RuleID == "hardcoded-credential" {
			target = &rep.Findings[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("a literal Password: property was not flagged; findings: %v", ruleIDs(rep.Findings))
	}
	prop, _ := target.Metadata["property"].(string)
	if prop != "Password" {
		t.Fatalf("metadata.property = %q, want Password", prop)
	}
	if target.AutoFix != "replace-with-variable" {
		t.Fatalf("AutoFix = %q, want replace-with-variable", target.AutoFix)
	}

	var block *models.Block
	walkBlocks(doc, func(b *models.Block) {
		if b.ID == target.BlockID {
			block = b
		}
	})
	if block == nil {
		t.Fatal("finding's block not found in the parsed flow")
	}
	patched := ApplyPatch(src, ReplaceWithVariablePatch(block, prop))
	if strings.Contains(patched, "Wint3r2024") || !strings.Contains(patched, "%input_password%") {
		t.Fatalf("fix did not replace the credential:\n%s", patched)
	}

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("ParseText (patched): %v", err)
	}
	for _, f := range RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil).Findings {
		if f.RuleID == "hardcoded-credential" {
			t.Fatalf("hardcoded-credential still fires after the fix: %+v", f)
		}
	}
}
