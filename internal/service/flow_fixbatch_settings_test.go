package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/testutil"

	"pad-core/models"
	"pad-core/parser"
)

// fixBatchSrc trips several auto-fixable rules on one HTTP action.
const fixBatchSrc = "#Region \"Main\"\n" +
	"HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x'\n" +
	"#EndRegion\n"

func newFixBatchSvc(t *testing.T, settings SettingsProvider) (*FlowService, *models.FlowDocument) {
	t.Helper()
	doc, err := parser.ParseText(fixBatchSrc, "Main.txt", int64(len(fixBatchSrc)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	doc.FilePath = ""
	doc.Source = fixBatchSrc
	doc.RebuildIndexes()

	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	return NewFlowService(&testutil.CountingNotifier{}, settings, ldp, &testutil.FakeBackend{}, nil, nil), doc
}

// TestApplyFixBatch_HonorsDisabledRule is the server half of the fix-loop
// configuration contract. The batch loop used to analyze with hardcoded
// defaults, so "fix all" could rewrite a flow to resolve a finding the
// deployment had disabled — the analysis reported nothing and the fixer changed
// the file anyway.
func TestApplyFixBatch_HonorsDisabledRule(t *testing.T) {
	store := newTestSettingsStore(t)

	// Baseline: the rule is fixable before it is disabled, so the assertion
	// below cannot pass just because the fixture stopped firing.
	svc, doc := newFixBatchSvc(t, store)
	if _, applied, err := svc.ApplyFixBatch(context.Background(), doc, map[string]bool{"missing-timeout": true}, 10); err != nil {
		t.Fatalf("baseline ApplyFixBatch: %v", err)
	} else if applied == 0 {
		t.Fatal("fixture has rotted: missing-timeout was not fixed with default settings")
	}

	settings := store.Get()
	if settings.Analysis.Rules == nil {
		settings.Analysis.Rules = map[string]models.RuleConfig{}
	}
	settings.Analysis.Rules["missing-timeout"] = models.RuleConfig{Enabled: false}
	if err := store.Update(*settings); err != nil {
		t.Fatalf("Update settings: %v", err)
	}

	svc2, doc2 := newFixBatchSvc(t, store)
	_, applied, err := svc2.ApplyFixBatch(context.Background(), doc2, map[string]bool{"missing-timeout": true}, 10)
	if err != nil {
		t.Fatalf("ApplyFixBatch: %v", err)
	}
	if applied != 0 {
		t.Errorf("applied %d fix(es) for a rule the settings disabled", applied)
	}
}

// TestFixOptions_NilSettingsProviderIsSafe covers the desktop/test wiring where
// neither a resolver nor a settings provider is present: the loop must fall
// back to the built-in defaults rather than panicking on a nil provider.
func TestFixOptions_NilSettingsProviderIsSafe(t *testing.T) {
	svc, doc := newFixBatchSvc(t, nil)
	opts := svc.fixOptions(context.Background(), doc, nil, 10)
	if opts.Settings != nil {
		t.Errorf("nil provider should leave Settings nil (the loop reads that as defaults), got %+v", opts.Settings)
	}
	if _, applied, err := svc.ApplyFixBatch(context.Background(), doc, nil, 10); err != nil {
		t.Fatalf("ApplyFixBatch with nil settings provider: %v", err)
	} else if applied == 0 {
		t.Error("expected the default rule set to still fix something")
	}
}
