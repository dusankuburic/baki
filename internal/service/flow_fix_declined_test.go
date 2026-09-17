package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/testutil"

	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"
)

const sensitiveExposureSrc = "#Region \"Main\"\n" +
	"SET Password TO 'secret123'\n" +
	"Text.WriteToFile TextToWrite: %Password% FilePath: 'log.txt'\n" +
	"#EndRegion\n"

func sensitiveExposureFinding(t *testing.T, doc *models.FlowDocument) *models.Finding {
	t.Helper()
	report := analyzer.RunAnalysis(doc, analyzer.AllRules(), models.DefaultSettings(), nil)
	for i := range report.Findings {
		if report.Findings[i].RuleID == "sensitive-exposure" {
			return &report.Findings[i]
		}
	}
	t.Fatalf("fixture has rotted: no sensitive-exposure finding in %+v", report.Findings)
	return nil
}

// TestFix_EmptyPatchIsDeclined pins C2: the finding card used to send
// mask-sensitive-variable without the finding's `variable` metadata, the fixer
// returned an empty patch, and both routes reported success — the preview
// diff was identical and "Apply fix" toasted success on an untouched flow. An
// empty patch is now ErrInvalidInput (400) on preview and apply, in cloud mode
// and on desktop, and the same fix WITH the variable still works.
func TestFix_EmptyPatchIsDeclined(t *testing.T) {
	ctx := context.Background()

	t.Run("cloud", func(t *testing.T) {
		doc, err := parser.ParseText(sensitiveExposureSrc, "Main.txt", int64(len(sensitiveExposureSrc)))
		if err != nil {
			t.Fatalf("ParseText: %v", err)
		}
		doc.FilePath = ""
		doc.Source = sensitiveExposureSrc
		doc.RebuildIndexes()
		ldp := NewLocalDocumentProvider()
		ldp.SetCurrentDoc(doc)
		svc := NewFlowService(&testutil.CountingNotifier{}, nil, ldp, &testutil.FakeBackend{}, nil, nil)
		f := sensitiveExposureFinding(t, doc)

		if _, err := svc.PreviewFix(doc, f.BlockID, "mask-sensitive-variable", f.RuleID, "", ""); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "fixer declined") {
			t.Errorf("PreviewFix without variable: err = %v, want ErrInvalidInput \"fixer declined\"", err)
		}
		if _, err := svc.ApplyFix(ctx, doc, f.BlockID, "mask-sensitive-variable", f.RuleID, "", "", true); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ApplyFix without variable: err = %v, want ErrInvalidInput", err)
		}

		variable, _ := f.Metadata["variable"].(string)
		res, err := svc.PreviewFix(doc, f.BlockID, "mask-sensitive-variable", f.RuleID, variable, "")
		if err != nil {
			t.Fatalf("PreviewFix with variable: %v", err)
		}
		if res.Original == res.Patched {
			t.Error("PreviewFix with variable produced an identical diff")
		}
	})

	t.Run("desktop", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "Main.txt")
		if err := os.WriteFile(path, []byte(sensitiveExposureSrc), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, nil)
		doc, err := svc.LoadFlowFromPath(path)
		if err != nil {
			t.Fatalf("LoadFlowFromPath: %v", err)
		}
		f := sensitiveExposureFinding(t, doc)

		if _, err := svc.PreviewFix(doc, f.BlockID, "mask-sensitive-variable", f.RuleID, "", ""); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("PreviewFix without variable: err = %v, want ErrInvalidInput", err)
		}
		if _, err := svc.ApplyFix(ctx, doc, f.BlockID, "mask-sensitive-variable", f.RuleID, "", "", true); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ApplyFix without variable: err = %v, want ErrInvalidInput", err)
		}
		if got, _ := os.ReadFile(path); string(got) != sensitiveExposureSrc { // #nosec G304 -- test temp dir
			t.Errorf("declined fix modified the file:\n%s", got)
		}
	})
}
