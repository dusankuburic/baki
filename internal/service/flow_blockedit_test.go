package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
	"pad-core/parser"
)

const editFlowSrc = `#Region "Main"
    SET Count TO 0
    HTTPClient.InvokeUrl Url: $'''https://target''' Method: HTTPClient.Method.GET
    WebAutomation.CloseWebBrowser BrowserInstance: %Browser%
    ON ERROR REPEAT 3 TIMES WAIT 3
    IF %Count% = 0
        Display.ShowMessageBox Message: $'''inside'''
    END
    COMMENT  trailing marker
#EndRegion
`

func blockIDByURL(doc *models.FlowDocument, url string) string {
	for i := range doc.Subflows {
		for j := range doc.Subflows[i].Blocks {
			b := &doc.Subflows[i].Blocks[j]
			if b.Properties["Url"] == url {
				return b.ID
			}
		}
	}
	return ""
}

func httpURLBlockIDRecursive(doc *models.FlowDocument, url string) string {
	var walk func(blocks []models.Block) string
	walk = func(blocks []models.Block) string {
		for i := range blocks {
			if blocks[i].Properties["Url"] == url {
				return blocks[i].ID
			}
			if id := walk(blocks[i].Children); id != "" {
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

func newDesktopEditSvc(t *testing.T) (*FlowService, *models.FlowDocument) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Main.txt")
	if err := os.WriteFile(path, []byte(editFlowSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := parser.ParseText(editFlowSrc, "Main.txt", int64(len(editFlowSrc)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc.FilePath = path
	doc.RebuildIndexes()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), ldp, nil, nil, nil)
	return svc, doc
}

func blockCount(doc *models.FlowDocument) int {
	n := 0
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			if blocks[i].Type != models.BlockTypeEnd {
				n++
			}
			walk(blocks[i].Children)
		}
	}
	for i := range doc.Subflows {
		walk(doc.Subflows[i].Blocks)
	}
	return n
}

func sourceOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestRemoveBlock_Desktop pins the leaf case: the target block AND its
// trailing inline-retry directive disappear (the directive folds into the
// PRECEDING block at parse — here CloseWebBrowser owns it); siblings
// survive; a snapshot captured the pre-edit state (verified by restore).
func TestRemoveBlock_Desktop(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	before := blockCount(doc)
	var target string
	for i := range doc.Subflows[0].Blocks {
		if doc.Subflows[0].Blocks[i].Properties["_retryCount"] != "" {
			target = doc.Subflows[0].Blocks[i].ID
		}
	}
	if target == "" {
		t.Fatal("fixture has no retry-carrying block")
	}

	updated, err := svc.RemoveBlock(context.Background(), doc, target)
	if err != nil {
		t.Fatalf("RemoveBlock: %v", err)
	}
	src := sourceOf(t, doc.FilePath)
	if strings.Contains(src, "CloseWebBrowser") {
		t.Errorf("target block still present:\n%s", src)
	}
	if strings.Contains(src, "ON ERROR REPEAT") {
		t.Errorf("trailing retry directive not removed with its action:\n%s", src)
	}
	if !strings.Contains(src, "SET Count TO 0") || !strings.Contains(src, "trailing marker") || !strings.Contains(src, "https://target") {
		t.Errorf("siblings lost:\n%s", src)
	}
	if got := blockCount(updated); got != before-1 {
		t.Errorf("block count %d, want %d", got, before-1)
	}
	// Undo: the pre-edit snapshot restores the removed block.
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(snaps))
	}
	restored, err := svc.RestoreSourceSnapshot(context.Background(), doc, snaps[0].ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if blockCount(restored) != before || !strings.Contains(sourceOf(t, doc.FilePath), "CloseWebBrowser") {
		t.Errorf("restore did not bring the removed block back")
	}
}

// TestRemoveBlock_ContainerSpansDescendants: removing an IF removes its body
// AND its END marker (the END parses as a child, so BlockSpan covers it) —
// the file stays balanced.
func TestRemoveBlock_ContainerSpansDescendants(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	var ifID string
	for i := range doc.Subflows[0].Blocks {
		if doc.Subflows[0].Blocks[i].Type == models.BlockTypeCondition {
			ifID = doc.Subflows[0].Blocks[i].ID
		}
	}
	if ifID == "" {
		t.Fatal("fixture has no IF block")
	}

	if _, err := svc.RemoveBlock(context.Background(), doc, ifID); err != nil {
		t.Fatalf("RemoveBlock(container): %v", err)
	}
	src := sourceOf(t, doc.FilePath)
	if strings.Contains(src, "inside") {
		t.Errorf("container body not removed:\n%s", src)
	}
	// Region end marker must survive exactly once (a leaked END would
	// unbalance the region).
	if strings.Count(src, "#EndRegion") != 1 || strings.Count(src, "END\n") != 0 {
		t.Errorf("container END leaked (unbalanced region):\n%s", src)
	}
	// The re-parsed doc has no dangling structure: the parse gate inside
	// PatchFlow already asserted no NEW parse errors.
	reparsed, err := parser.ParseText(src, "x", int64(len(src)))
	if err != nil || len(reparsed.ParseErrors) != 0 {
		t.Errorf("post-edit source has parse errors: %v %+v", err, reparsed.ParseErrors)
	}
}

// TestDuplicateBlock_Desktop: the copy (verbatim lines, same indent) lands
// directly after the original; block count +1; the original survives.
func TestDuplicateBlock_Desktop(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	before := blockCount(doc)
	target := blockIDByURL(doc, "https://target")

	updated, err := svc.DuplicateBlock(context.Background(), doc, target)
	if err != nil {
		t.Fatalf("DuplicateBlock: %v", err)
	}
	src := sourceOf(t, doc.FilePath)
	if n := strings.Count(src, "https://target"); n != 2 {
		t.Errorf("target line count = %d, want 2:\n%s", n, src)
	}
	if got := blockCount(updated); got != before+1 {
		t.Errorf("block count %d, want %d", got, before+1)
	}
	// The duplicate is functional: two distinct block IDs share the URL.
	if httpURLBlockIDRecursive(updated, "https://target") == "" {
		t.Error("duplicated block missing from re-parse")
	}
}

// TestBlockEdit_CloudBridgeAlignment: an INGESTED flow (no stored Source,
// blank lines shifting canonical line numbers) removes the block the user
// pointed at — not its neighbor (the same alignment contract as the fix
// bridge, exercised for block edits).
func TestBlockEdit_CloudBridgeAlignment(t *testing.T) {
	src := "#Region \"Main\"\n\n    SET First TO 1\n\n\n    HTTPClient.InvokeUrl Url: $'''https://second''' Method: HTTPClient.Method.GET\n\n#EndRegion\n"
	doc, err := parser.ParseText(src, "Blanky", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc.Source = "" // ingested bridge
	doc.FilePath = ""
	doc.RebuildIndexes()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, ldp, &testutil.FakeBackend{}, nil, nil)

	updated, err := svc.RemoveBlock(context.Background(), doc, httpURLBlockIDRecursive(doc, "https://second"))
	if err != nil {
		t.Fatalf("RemoveBlock(cloud bridge): %v", err)
	}
	if httpURLBlockIDRecursive(updated, "https://second") != "" {
		t.Error("targeted block survived the bridge removal")
	}
	// The neighbor survives.
	found := false
	for i := range updated.Subflows {
		for j := range updated.Subflows[i].Blocks {
			if updated.Subflows[i].Blocks[j].Properties["_var"] == "First" {
				found = true
			}
		}
	}
	if !found {
		t.Error("neighbor block was removed — bridge misalignment")
	}
}

// TestBlockEdit_StaleFileRefused: a block span computed against the doc's
// parse must NOT be applied to bytes that changed on disk underneath — the
// line-content drift guard rejects with an actionable "reload" error instead
// of splicing the wrong lines.
func TestBlockEdit_StaleFileRefused(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	target := blockIDByURL(doc, "https://target")
	// Rewrite the file so the target's span line holds different content.
	if err := os.WriteFile(doc.FilePath, []byte("#Region \"Main\"\n    SET Different TO 1\n    SET Another TO 2\n#EndRegion\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RemoveBlock(context.Background(), doc, target); err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Errorf("stale-file edit should be refused with the reload error, got %v", err)
	}
}
