package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
)

// TestResourceLeakRule_ConnectionAndBrowserPairs covers the lifecycles the rule
// description promised but never checked: SQL/ODBC connections, FTP sessions
// and browser instances. For each pair: open without close is a finding, open
// plus a close that references the handle is not, and a close of a DIFFERENT
// handle does not count.
func TestResourceLeakRule_ConnectionAndBrowserPairs(t *testing.T) {
	rule := &ResourceLeakRule{}
	cases := []struct {
		name, open, closeRaw, label, wantClose string
	}{
		{"SQL connection", "Database.OpenSQLConnection", "Database.CloseConnection", "database connection", "Database.Close"},
		{"ODBC connection", "Database.OpenODBCConnection", "Database.Close", "database connection", "Database.Close"},
		{"FTP connection", "FTP.OpenConnection", "FTP.CloseConnection", "FTP connection", "FTP.CloseConnection"},
		{"secure FTP connection", "FTP.OpenSecureConnection", "FTP.CloseConnection", "FTP connection", "FTP.CloseConnection"},
		{"exporter casing of FTP", "Ftp.OpenFtpConnection", "Ftp.CloseConnection", "FTP connection", "FTP.CloseConnection"},
		{"Chrome", "WebAutomation.LaunchChrome", "WebAutomation.CloseWebBrowser", "web browser instance", "WebAutomation.CloseWebBrowser"},
		{"Edge closed via window", "WebAutomation.LaunchEdge", "WebAutomation.CloseWindow", "web browser instance", "WebAutomation.CloseWebBrowser"},
		{"generic browser", "WebAutomation.LaunchBrowser", "WebAutomation.CloseBrowser", "web browser instance", "WebAutomation.CloseWebBrowser"},
		{"Firefox", "WebAutomation.LaunchFirefox", "WebAutomation.CloseWebBrowser", "web browser instance", "WebAutomation.CloseWebBrowser"},
	}
	newFlow := func(blocks ...models.Block) *models.FlowDocument {
		return &models.FlowDocument{ID: "t", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}}}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opener := makeBlock("b1", "open", models.BlockTypeAction, tc.open, 0)
			opener.Properties = map[string]string{"_output": "Handle"}
			opener.SubflowID = "sf1"

			t.Run("open without close", func(t *testing.T) {
				got := rule.Check(opener, buildContext(newFlow(*opener), nil))
				if len(got) != 1 {
					t.Fatalf("expected 1 finding, got %d: %+v", len(got), got)
				}
				if r, _ := got[0].Metadata["resource"].(string); r != tc.label {
					t.Errorf("metadata.resource = %q, want %q", r, tc.label)
				}
				if !strings.Contains(got[0].Suggestion, tc.wantClose) {
					t.Errorf("suggestion should name %q:\n%s", tc.wantClose, got[0].Suggestion)
				}
			})

			t.Run("open then close of the handle", func(t *testing.T) {
				closer := makeBlock("b2", "close", models.BlockTypeAction, tc.closeRaw, 0)
				closer.Properties = map[string]string{"Instance": "%Handle%"}
				closer.SubflowID = "sf1"
				if got := rule.Check(opener, buildContext(newFlow(*opener, *closer), nil)); len(got) != 0 {
					t.Fatalf("expected 0 findings, got %d: %+v", len(got), got)
				}
			})

			t.Run("close of a different handle does not count", func(t *testing.T) {
				closer := makeBlock("b2", "close", models.BlockTypeAction, tc.closeRaw, 0)
				closer.Properties = map[string]string{"Instance": "%Other%"}
				closer.SubflowID = "sf1"
				if got := rule.Check(opener, buildContext(newFlow(*opener, *closer), nil)); len(got) != 1 {
					t.Fatalf("expected 1 finding, got %d: %+v", len(got), got)
				}
			})
		})
	}
}

// TestInsertClosePatch_BrowserNamesARealCloseAction: the fixer shares the pair
// table with the rule and emits the concrete close action for a browser
// launch, not the bare "WebAutomation.Close" match prefix.
func TestInsertClosePatch_BrowserNamesARealCloseAction(t *testing.T) {
	block := &models.Block{
		ID: "b1", Type: models.BlockTypeAction, RawType: "WebAutomation.LaunchChrome",
		LineNumber: 1, Properties: map[string]string{"_output": "Browser"},
	}
	patch := InsertClosePatch(block)
	if len(patch.Ops) != 1 || len(patch.Ops[0].Lines) != 1 {
		t.Fatalf("expected one inserted line, got %+v", patch)
	}
	line := patch.Ops[0].Lines[0]
	if !strings.Contains(line, "WebAutomation.CloseWebBrowser") || !strings.Contains(line, "%Browser%") {
		t.Errorf("close line = %q", line)
	}
}
