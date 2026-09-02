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

const bulkFlowSrc = `#Region "Main"
    SET A TO 1
    SET B TO 2
    COMMENT  first marker
    SET C TO 3
    COMMENT  second marker
    IF %A% = 1
        SET D TO 4
    END
    LABEL 'Done'
    GOTO 'Done'
#EndRegion
`

func newBulkEditSvc(t *testing.T) (*FlowService, *models.FlowDocument) {
	t.Helper()
	return newDesktopEditSvcWith(t, bulkFlowSrc)
}

// newDesktopEditSvcWith is editFlowSrc-parameterized (the shared fixture
// builder hardcodes editFlowSrc).
func newDesktopEditSvcWith(t *testing.T, src string) (*FlowService, *models.FlowDocument) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Main.txt")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc.FilePath = path
	doc.RebuildIndexes()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	return NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), ldp, nil, nil, nil), doc
}

// idsByRawType returns the ID of the nth block (preorder) with rawType.
func idsByRawType(doc *models.FlowDocument, rawType string, nth int) string {
	found := 0
	result := ""
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			if result != "" {
				return
			}
			if blocks[i].RawType == rawType {
				found++
				if found == nth {
					result = blocks[i].ID
					return
				}
			}
			walk(blocks[i].Children)
		}
	}
	walk(doc.Subflows[0].Blocks)
	return result
}

func TestRemoveBlocks_Desktop(t *testing.T) {
	svc, doc := newBulkEditSvc(t)

	setA := idsByRawType(doc, "SET", 1)
	setB := idsByRawType(doc, "SET", 2)
	cmt1 := idsByRawType(doc, "COMMENT", 1)

	// Delete SET A, SET B, and the first COMMENT in ONE call.
	updated, err := svc.RemoveBlocks(context.Background(), doc, []string{setA, setB, cmt1})
	if err != nil {
		t.Fatalf("RemoveBlocks: %v", err)
	}
	src := sourceOf(t, doc.FilePath)
	if strings.Contains(src, "SET A TO 1") || strings.Contains(src, "SET B TO 2") || strings.Contains(src, "first marker") {
		t.Errorf("targets survived:\n%s", src)
	}
	// Everything else intact — including the container and its body.
	for _, want := range []string{"SET C TO 3", "second marker", "IF %A% = 1", "SET D TO 4", "END", "LABEL 'Done'", "GOTO 'Done'"} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q after bulk delete:\n%s", want, src)
		}
	}
	// Re-parse keeps structure: 3 fewer top-level blocks (the IF's END is
	// nested inside it, not top-level).
	if got := len(updated.Subflows[0].Blocks); got != 5 { // C, comment2, IF, LABEL, GOTO
		t.Errorf("top-level blocks = %d, want 5", got)
	}
}

func TestRemoveBlocks_Refusals(t *testing.T) {
	svc, doc := newBulkEditSvc(t)
	setA := idsByRawType(doc, "SET", 1)
	ifBlock := idsByRawType(doc, "IF", 1)
	setD := idsByRawType(doc, "SET", 4) // inside the IF

	if _, err := svc.RemoveBlocks(context.Background(), doc, nil); err == nil || !strings.Contains(err.Error(), "no blocks selected") {
		t.Errorf("empty selection should refuse, got %v", err)
	}
	if _, err := svc.RemoveBlocks(context.Background(), doc, []string{"missing"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown id should refuse, got %v", err)
	}
	// Ancestor + descendant overlap.
	if _, err := svc.RemoveBlocks(context.Background(), doc, []string{ifBlock, setD}); err == nil || !strings.Contains(err.Error(), "descendant") {
		t.Errorf("ancestor+descendant should refuse, got %v", err)
	}
	// Source unchanged after refusals.
	if src := sourceOf(t, doc.FilePath); !strings.Contains(src, "SET A TO 1") {
		t.Errorf("a refusal must not write:\n%s", src)
	}
	_ = setA
}

func TestRemoveBlocks_CloudBridge(t *testing.T) {
	doc, err := parser.ParseText(bulkFlowSrc, "Bulk", int64(len(bulkFlowSrc)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc.Source = ""
	doc.FilePath = ""
	doc.RebuildIndexes()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, ldp, &testutil.FakeBackend{}, nil, nil)

	setA := idsByRawType(doc, "SET", 1)
	cmt1 := idsByRawType(doc, "COMMENT", 1)
	updated, err := svc.RemoveBlocks(context.Background(), doc, []string{setA, cmt1})
	if err != nil {
		t.Fatalf("cloud RemoveBlocks: %v", err)
	}
	if strings.Contains(updated.Source, "SET A TO 1") || strings.Contains(updated.Source, "first marker") {
		t.Errorf("targets survived in canonical source:\n%s", updated.Source)
	}
	// The canonical serializer writes labels bare.
	if !strings.Contains(updated.Source, "LABEL Done") || !strings.Contains(updated.Source, "GOTO Done") {
		t.Errorf("unrelated blocks lost:\n%s", updated.Source)
	}
}

func TestRenameBlock_LabelAndGotoRefs(t *testing.T) {
	svc, doc := newBulkEditSvc(t)
	label := idsByRawType(doc, "LABEL", 1)

	updated, refs, err := svc.RenameBlock(context.Background(), doc, label, "Finished")
	if err != nil {
		t.Fatalf("RenameBlock: %v", err)
	}
	if refs != 1 {
		t.Errorf("gotoRefs = %d, want 1", refs)
	}
	src := sourceOf(t, doc.FilePath)
	if !strings.Contains(src, "LABEL 'Finished'") || !strings.Contains(src, "GOTO 'Finished'") {
		t.Errorf("rename did not propagate to label + goto:\n%s", src)
	}
	if strings.Contains(src, "'Done'") {
		t.Errorf("old name survived:\n%s", src)
	}
	// The re-parse sees the new name on both blocks.
	if idsByRawType(updated, "LABEL", 1) == "" {
		t.Fatal("label vanished from updated doc")
	}
	var label2, goto2 *models.Block
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			switch blocks[i].RawType {
			case "LABEL":
				label2 = &blocks[i]
			case "GOTO":
				goto2 = &blocks[i]
			}
			walk(blocks[i].Children)
		}
	}
	walk(updated.Subflows[0].Blocks)
	if label2 == nil || label2.Name != "Finished" {
		t.Errorf("parsed label name = %q, want Finished", label2.Name)
	}
	if goto2 == nil || goto2.Name != "Finished" {
		t.Errorf("parsed goto name = %q, want Finished", goto2.Name)
	}
}

func TestRenameBlock_Comment(t *testing.T) {
	svc, doc := newBulkEditSvc(t)
	cmt := idsByRawType(doc, "COMMENT", 2) // "second marker"

	if _, _, err := svc.RenameBlock(context.Background(), doc, cmt, "renamed marker"); err != nil {
		t.Fatalf("RenameBlock(comment): %v", err)
	}
	src := sourceOf(t, doc.FilePath)
	if !strings.Contains(src, "COMMENT  renamed marker") {
		t.Errorf("comment not renamed:\n%s", src)
	}
	if strings.Contains(src, "second marker") {
		t.Errorf("old comment text survived:\n%s", src)
	}
}

func TestRenameBlock_Refusals(t *testing.T) {
	svc, doc := newBulkEditSvc(t)
	setA := idsByRawType(doc, "SET", 1)
	label := idsByRawType(doc, "LABEL", 1)

	if _, _, err := svc.RenameBlock(context.Background(), doc, setA, "Nope"); err == nil || !strings.Contains(err.Error(), "derived") {
		t.Errorf("action rename should refuse with derived-name guidance, got %v", err)
	}
	if _, _, err := svc.RenameBlock(context.Background(), doc, label, ""); err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("empty name should refuse, got %v", err)
	}
	if _, _, err := svc.RenameBlock(context.Background(), doc, label, "bad'name"); err == nil || !strings.Contains(err.Error(), "quotes") {
		t.Errorf("quote in label name should refuse, got %v", err)
	}
	if src := sourceOf(t, doc.FilePath); !strings.Contains(src, "LABEL 'Done'") {
		t.Errorf("refusals must not write:\n%s", src)
	}
}

func TestRenameBlock_CloudBridge_BareLabels(t *testing.T) {
	// Canon serializer emits labels BARE — the rename must match that form.
	doc, err := parser.ParseText(bulkFlowSrc, "Bulk", int64(len(bulkFlowSrc)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc.Source = ""
	doc.FilePath = ""
	doc.RebuildIndexes()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, ldp, &testutil.FakeBackend{}, nil, nil)

	label := idsByRawType(doc, "LABEL", 1)
	updated, refs, err := svc.RenameBlock(context.Background(), doc, label, "Finished")
	if err != nil {
		t.Fatalf("cloud rename: %v", err)
	}
	if refs != 1 {
		t.Errorf("gotoRefs = %d, want 1", refs)
	}
	if !strings.Contains(updated.Source, "LABEL Finished") || !strings.Contains(updated.Source, "GOTO Finished") {
		t.Errorf("bare-form rename failed:\n%s", updated.Source)
	}
	if strings.Contains(updated.Source, "Done") {
		t.Errorf("old name survived:\n%s", updated.Source)
	}
}
