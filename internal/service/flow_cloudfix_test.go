package service

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
	"pad-core/parser"
)

const folderMainSrc = `#Region "Main"

    HTTPClient.InvokeUrl Url: $'''https://main''' Method: HTTPClient.Method.GET

#EndRegion
`
const folderUtilSrc = `#Region "Util"

    HTTPClient.InvokeUrl Url: $'''https://util''' Method: HTTPClient.Method.GET

#EndRegion
`

// cloudFolderDoc builds an uploaded CLOUD folder flow: parsed via ParseFiles
// (per-subflow SourceFile set), Source empty (uploads never store per-file
// source), no FilePath.
func cloudFolderDoc(t *testing.T) *models.FlowDocument {
	t.Helper()
	files := map[string]string{"Main.txt": folderMainSrc, "Util.txt": folderUtilSrc}
	doc, err := parser.ParseFiles(files, "Folder Flow")
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	doc.Source = ""
	doc.FilePath = ""
	doc.RebuildIndexes()
	return doc
}

func newCloudFixSvc(t *testing.T, doc *models.FlowDocument) *FlowService {
	t.Helper()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	return NewFlowService(&testutil.CountingNotifier{}, nil, ldp, &testutil.FakeBackend{}, nil, nil)
}

func httpBlockID(doc *models.FlowDocument, nameFragment string) string {
	// Recursive: after a wrap the action nests inside the error handler, so a
	// top-level scan would miss it.
	var walk func(blocks []models.Block) string
	walk = func(blocks []models.Block) string {
		for i := range blocks {
			b := &blocks[i]
			if b.RawType == "HTTPClient.InvokeUrl" && strings.Contains(b.Properties["Url"], nameFragment) {
				return b.ID
			}
			if id := walk(b.Children); id != "" {
				return id
			}
		}
		return ""
	}
	for i := range doc.Subflows {
		if id := walk(doc.Subflows[i].Blocks); id != "" {
			return id
		}
	}
	return ""
}

// TestCloudFolderFix_SingleApply pins R3-3's single-fix path: a folder flow's
// PreviewFix returns the TARGET member file's text (labelled), ApplyFix
// patches that file only, and the persisted doc stays a folder with the other
// member untouched.
func TestCloudFolderFix_SingleApply(t *testing.T) {
	doc := cloudFolderDoc(t)
	svc := newCloudFixSvc(t, doc)
	mainID := httpBlockID(doc, "main")
	if mainID == "" {
		t.Fatal("fixture has no main-file HTTP block")
	}

	// Preview: the diff is the Main file's text, labelled as such.
	prev, err := svc.PreviewFix(doc, mainID, "wrap-error-handler", "unhandled-error", "", "")
	if err != nil {
		t.Fatalf("PreviewFix: %v", err)
	}
	if prev.File != "Main.txt" {
		t.Errorf("preview file = %q, want Main.txt", prev.File)
	}
	if !strings.Contains(prev.Original, "https://main") || strings.Contains(prev.Original, "https://util") {
		t.Errorf("preview should show ONLY the Main file's source")
	}
	if !strings.Contains(prev.Patched, "ON BLOCK ERROR") {
		t.Errorf("patched preview missing the handler wrap:\n%s", prev.Patched)
	}

	updated, err := svc.ApplyFix(context.Background(), doc, mainID, "wrap-error-handler", "unhandled-error", "", "")
	if err != nil {
		t.Fatalf("ApplyFix: %v", err)
	}
	if !updated.IsFolder {
		t.Fatal("folder shape lost after fix")
	}
	// Main gained the handler; Util is untouched.
	mainSf := subflowByFile(updated, "Main.txt")
	utilSf := subflowByFile(updated, "Util.txt")
	if mainSf == nil || utilSf == nil {
		t.Fatalf("member files lost: %+v", updated.Files)
	}
	if !hasHandlerWrap(mainSf) {
		t.Error("Main's block was not wrapped")
	}
	if hasHandlerWrap(utilSf) {
		t.Error("Util's block was wrongly modified")
	}
	// Source stays empty (folder persistence keeps the bridge model).
	if updated.Source != "" {
		t.Errorf("folder Source should stay empty, got %d bytes", len(updated.Source))
	}
}

// TestCloudFolderFix_Batch pins the batch path: the loop runs per member
// file, both files get fixed, folder shape survives, and the undo snapshot
// restores the whole folder.
func TestCloudFolderFix_Batch(t *testing.T) {
	doc := cloudFolderDoc(t)
	svc := newCloudFixSvc(t, doc)

	updated, applied, err := svc.ApplyFixBatch(context.Background(), doc, nil, 10)
	if err != nil {
		t.Fatalf("ApplyFixBatch: %v", err)
	}
	if applied == 0 {
		t.Fatal("expected fixes to land")
	}
	if !updated.IsFolder {
		t.Fatal("folder shape lost after batch")
	}
	if !hasHandlerWrap(subflowByFile(updated, "Main.txt")) || !hasHandlerWrap(subflowByFile(updated, "Util.txt")) {
		t.Error("expected both member files fixed")
	}

	// Undo: the pre-batch snapshot restores BOTH files' original state.
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) == 0 {
		t.Fatal("no snapshot captured")
	}
	restored, err := svc.RestoreSourceSnapshot(context.Background(), doc, snaps[len(snaps)-1].ID)
	if err != nil {
		t.Fatalf("RestoreSourceSnapshot: %v", err)
	}
	if !restored.IsFolder {
		t.Fatal("restore collapsed the folder")
	}
	if hasHandlerWrap(subflowByFile(restored, "Main.txt")) || hasHandlerWrap(subflowByFile(restored, "Util.txt")) {
		t.Error("restore did not roll both files back")
	}
}

// TestCloudBridge_LineAlignment is the R1-1b latent-bug regression: a flow
// parsed from source WITH BLANK LINES (canonical serialization drops them, so
// line numbers shift) gets its fix on the CORRECT block. The original bridge
// applied original-parse line numbers to canonical text — with the blank
// lines here it would have wrapped the WRONG line.
func TestCloudBridge_LineAlignment(t *testing.T) {
	// Two HTTP actions separated by blank lines; the fix targets the SECOND.
	src := "#Region \"Main\"\n\n    HTTPClient.InvokeUrl Url: $'''https://first''' Method: HTTPClient.Method.GET\n\n\n    COMMENT  spacer comment\n\n    HTTPClient.InvokeUrl Url: $'''https://second''' Method: HTTPClient.Method.GET\n\n#EndRegion\n"
	doc, err := parser.ParseText(src, "Blanky", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc.Source = "" // ingested bridge: no stored source
	doc.FilePath = ""
	doc.RebuildIndexes()

	svc := newCloudFixSvc(t, doc)
	secondID := httpBlockID(doc, "second")
	if secondID == "" {
		t.Fatal("fixture has no second block")
	}

	updated, err := svc.ApplyFix(context.Background(), doc, secondID, "wrap-error-handler", "unhandled-error", "", "")
	if err != nil {
		t.Fatalf("ApplyFix: %v", err)
	}
	// The SECOND block must be inside the handler; the FIRST must not.
	second := updated.BlocksByID[httpBlockID(updated, "second")]
	first := updated.BlocksByID[httpBlockID(updated, "first")]
	if second == nil || first == nil {
		t.Fatal("blocks missing after fix")
	}
	if !isInsideHandler(updated, second.ID) {
		t.Error("the TARGETED block was not wrapped — line misalignment")
	}
	if isInsideHandler(updated, first.ID) {
		t.Error("the WRONG block was wrapped — the exact bridge bug")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func subflowByFile(doc *models.FlowDocument, file string) *models.Subflow {
	for i := range doc.Subflows {
		if doc.Subflows[i].SourceFile == file {
			return &doc.Subflows[i]
		}
	}
	return nil
}

func hasHandlerWrap(sf *models.Subflow) bool {
	if sf == nil {
		return false
	}
	var walk func(blocks []models.Block) bool
	walk = func(blocks []models.Block) bool {
		for i := range blocks {
			if blocks[i].Type == models.BlockTypeErrorHandler {
				return true
			}
			if walk(blocks[i].Children) {
				return true
			}
		}
		return false
	}
	return walk(sf.Blocks)
}

// isInsideHandler reports whether blockID nests (transitively) under an
// error handler.
func isInsideHandler(doc *models.FlowDocument, blockID string) bool {
	var check func(blocks []models.Block, inside bool) bool
	check = func(blocks []models.Block, inside bool) bool {
		for i := range blocks {
			b := &blocks[i]
			now := inside || b.Type == models.BlockTypeErrorHandler
			if b.ID == blockID {
				return now
			}
			if check(b.Children, now) {
				return true
			}
		}
		return false
	}
	for i := range doc.Subflows {
		if check(doc.Subflows[i].Blocks, false) {
			return true
		}
	}
	return false
}
