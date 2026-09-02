package service

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
	"pad-core/parser"
)

// ingestedDoc mimics a padcloud-ingested flow: parsed subflows/content, no
// stored Source, no FilePath (cloud), single-file.
// newFlowTestService builds a FlowService with a local doc provider (holding
// doc) and a FakeBackend storage so the cloud persist path is exercised.
func newFlowTestService(t *testing.T, doc *models.FlowDocument) (*FlowService, *models.FlowDocument) {
	t.Helper()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	backend := &testutil.FakeBackend{}
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, ldp, backend, nil, nil)
	return svc, doc
}

func ingestedDoc(t *testing.T) *models.FlowDocument {
	t.Helper()
	src := "#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'''https://x''' Method: HTTPClient.Method.GET\n#EndRegion\n"
	doc, err := parser.ParseText(src, "Ingested Flow", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc.Source = "" // the ingester never sets it
	doc.FilePath = ""
	doc.RebuildIndexes()
	return doc
}

// TestPreviewFix_IngestedCloudFlow pins R1-1b: an ingested cloud flow (parsed
// content, no stored Source) previews against the SERIALIZED form instead of
// erroring "source not available". The preview round-trips: the patched text
// is valid PAD source derived from the serializer.
func TestPreviewFix_IngestedCloudFlow(t *testing.T) {
	svc, doc := newFlowTestService(t, ingestedDoc(t))

	// Find the HTTP action's finding (unhandled-error is auto-fixable).
	var blockID string
	for _, sf := range doc.Subflows {
		for i := range sf.Blocks {
			if sf.Blocks[i].RawType == "HTTPClient.InvokeUrl" {
				blockID = sf.Blocks[i].ID
			}
		}
	}
	if blockID == "" {
		t.Fatal("fixture has no HTTP action")
	}

	res, err := svc.PreviewFix(doc, blockID, "wrap-error-handler", "unhandled-error", "", "")
	if err != nil {
		t.Fatalf("PreviewFix on ingested flow: %v", err)
	}
	if res.Original == "" || !strings.Contains(res.Original, "HTTPClient.InvokeUrl") {
		t.Errorf("Original should be the serialized source:\n%s", res.Original)
	}
	if !strings.Contains(res.Patched, "ON BLOCK ERROR") {
		t.Errorf("Patched missing the handler wrap:\n%s", res.Patched)
	}
	// The patched text must itself be parseable PAD (the apply path re-parses).
	if _, err := parser.ParseText(res.Patched, "p", int64(len(res.Patched))); err != nil {
		t.Fatalf("patched source does not re-parse: %v", err)
	}
}

// TestApplyFixBatch_IngestedCloudFlow: the batch path also falls back to the
// serializer, applies fixes, and — via persistCloudSource — leaves the flow
// carrying real stored Source (the one-time bridge).
func TestApplyFixBatch_IngestedCloudFlow(t *testing.T) {
	svc, doc := newFlowTestService(t, ingestedDoc(t))

	updated, applied, err := svc.ApplyFixBatch(context.Background(), doc, nil, 10)
	if err != nil {
		t.Fatalf("ApplyFixBatch on ingested flow: %v", err)
	}
	if applied == 0 {
		t.Fatal("expected at least one fix")
	}
	if updated.Source == "" {
		t.Error("after the first fix the flow should carry stored Source")
	}
	if !strings.Contains(updated.Source, "ON BLOCK ERROR") {
		t.Errorf("stored source missing the applied fix:\n%s", updated.Source)
	}
}

// TestCloudFix_SaveSourceFolderGuard: saving the COMBINED source of a folder
// flow is rejected — persistCloudSource parses single text, which would
// silently collapse the folder into a single-file flow.
func TestCloudFix_SaveSourceFolderGuard(t *testing.T) {
	svc, doc := newFlowTestService(t, ingestedDoc(t))
	doc.IsFolder = true
	doc.Files = []models.FlowFileInfo{{Name: "Main.txt", Path: "Main.txt"}, {Name: "Util.txt", Path: "Util.txt"}}

	if _, err := svc.SaveSource(context.Background(), doc, "whatever"); err == nil || !strings.Contains(err.Error(), "collapse") {
		t.Errorf("folder SaveSource should be gated with the collapse warning, got %v", err)
	}
}
