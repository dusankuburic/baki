package service

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// batchLibraryFlow is a flow file whose subflow shared has the same body in every flow (a library finding) and
// whose subflow own differs per flow.
func batchLibraryFlow(shared, own string) string {
	return `#Region "` + shared + `"
    Display.ShowMessageBox Message: $'''Logging in''' Icon: Display.Icon.None
    SET Attempts TO 0
    LOOP WHILE %Attempts% < 3
        Variables.IncreaseVariable Value: 1 Name: Attempts
    END
#EndRegion
#Region "` + own + `"
    SET ` + own + ` TO 1
#EndRegion
`
}

func batchLibraryDocs(t *testing.T) []*models.FlowDocument {
	t.Helper()
	dir := t.TempDir()
	var docs []*models.FlowDocument
	for name, text := range map[string]string{
		"invoices.txt": batchLibraryFlow("Login", "Invoices"),
		"orders.txt":   batchLibraryFlow("SignIn", "Orders"),
	} {
		doc, err := parser.ParseText(text, name, int64(len(text)))
		if err != nil {
			t.Fatal(err)
		}
		doc.FilePath = filepath.Join(dir, name)
		docs = append(docs, doc)
	}
	return docs
}

// The desktop folder batch runs the library rules over the folder's flows: a subflow copied between two flows is
// reported in batch.Library (preview, naming the batch's own flow ids), never in a flow's report.
func TestAnalyzeBatch_RunsTheLibraryRules(t *testing.T) {
	svc := newDesktopAnalysisService(t)
	docs := batchLibraryDocs(t)
	batch, err := svc.AnalyzeBatch(docs)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Library == nil {
		t.Fatal("the batch carries no library report")
	}
	if batch.Library.Flows != len(docs) || batch.Library.RulesRun != 1 {
		t.Errorf("library report covers %d flows with %d rules, want %d flows, 1 rule", batch.Library.Flows, batch.Library.RulesRun, len(docs))
	}
	ids := map[string]bool{}
	for _, r := range batch.Results {
		ids[r.FlowID] = true
		for _, f := range r.Report.Findings {
			if f.RuleID == "duplicate-subflow-across-flows" {
				t.Errorf("%s: a library finding entered the flow's report", r.FlowName)
			}
		}
	}
	if len(batch.Library.Findings) != 2 {
		t.Fatalf("library findings = %+v, want one per copy of the shared subflow (2)", batch.Library.Findings)
	}
	for _, f := range batch.Library.Findings {
		if f.RuleID != "duplicate-subflow-across-flows" || f.Stage != "preview" || !ids[f.FlowID] {
			t.Errorf("library finding %+v: want the preview duplicate-subflow rule on one of the batch's flows", f)
		}
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Library *struct {
			Findings []json.RawMessage `json:"findings"`
		} `json:"library"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || wire.Library == nil || len(wire.Library.Findings) != 2 {
		t.Errorf("the batch JSON does not carry the library findings: %s", raw)
	}
}

// Settings that disable every library rule leave the batch without a library report (the JSON field is omitted).
func TestAnalyzeBatch_LibraryRulesFollowTheSettings(t *testing.T) {
	settings := deploymentSettings(map[string]models.RuleConfig{
		"duplicate-subflow-across-flows": {Enabled: false, Severity: "info"},
	})
	svc, err := NewAnalysisService(NilNotifier{}, settings, nil, AnalysisOptions{})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := svc.AnalyzeBatch(batchLibraryDocs(t))
	if err != nil {
		t.Fatal(err)
	}
	if batch.Library != nil {
		t.Errorf("a disabled library rule ran: %+v", batch.Library)
	}
	if len(batch.Results) != 2 {
		t.Errorf("the per-flow analyses did not run: %d results", len(batch.Results))
	}
}
