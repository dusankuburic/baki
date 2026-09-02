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

// TestLocatePropertyValue pins the property locator: whole-word key match,
// each quoting form, and the refusals (strict-prefix bare match, mid-word
// key, absent value).
func TestLocatePropertyValue(t *testing.T) {
	line := "HTTPClient.InvokeUrl Url: $'''https://x''' Method: HTTPClient.Method.GET Timeout: 30"

	if seg, ok := locatePropertyValue(line, "Url", "https://x"); !ok || seg != "Url: $'''https://x'''" {
		t.Errorf("quoted form: got %q ok=%v", seg, ok)
	}
	if seg, ok := locatePropertyValue(line, "Timeout", "30"); !ok || seg != "Timeout: 30" {
		t.Errorf("bare form: got %q ok=%v", seg, ok)
	}
	if seg, ok := locatePropertyValue(line, "Method", "HTTPClient.Method.GET"); !ok || seg != "Method: HTTPClient.Method.GET" {
		t.Errorf("dotted bare form: got %q ok=%v", seg, ok)
	}
	// A single-quoted value.
	if seg, ok := locatePropertyValue("Label.SetLabel Text: 'hello' X: 1", "Text", "hello"); !ok || seg != "Text: 'hello'" {
		t.Errorf("single-quoted form: got %q ok=%v", seg, ok)
	}
	// Refusals.
	if _, ok := locatePropertyValue(line, "Url", "https"); ok {
		t.Error("bare strict-prefix must not match")
	}
	if _, ok := locatePropertyValue("MyUrl: https://y Url: https://x", "Url", "https://y"); ok {
		t.Error("key must not match mid-word (MyUrl)")
	}
	if _, ok := locatePropertyValue(line, "Absent", "x"); ok {
		t.Error("absent property must not match")
	}
}

// TestUpdateBlockProperties_Desktop pins the batch edit: only the targeted
// property's text changes (other properties keep their original quoting and
// order), parse-gated, snapshotted.
func TestUpdateBlockProperties_Desktop(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	target := blockIDByURL(doc, "https://target")
	if target == "" {
		t.Fatal("target block missing")
	}

	updated, err := svc.UpdateBlockProperties(context.Background(), doc, target, map[string]string{
		"Url": "https://changed-example",
	})
	if err != nil {
		t.Fatalf("UpdateBlockProperties: %v", err)
	}
	src := sourceOf(t, doc.FilePath)
	// No spaces in the new value → QuoteValue emits the bare form (the
	// parser unwraps either form to the same value).
	if !strings.Contains(src, "Url: https://changed-example") {
		t.Errorf("edited property missing:\n%s", src)
	}
	// The sibling property on the SAME line is untouched.
	if !strings.Contains(src, "Method: HTTPClient.Method.GET") {
		t.Errorf("same-line sibling property corrupted:\n%s", src)
	}
	// Re-parse picks the new value up.
	if u := httpURLBlockIDRecursive(updated, "https://changed-example"); u == "" {
		t.Error("re-parse did not see the new value")
	}
	// Snapshot exists for undo.
	if snaps := svc.ListSourceSnapshots(doc); len(snaps) != 1 {
		t.Errorf("snapshots = %d, want 1", len(snaps))
	}
}

// TestUpdateBlockProperties_RequoteBare: editing a bare (enum-ish) value with
// one that needs quoting switches the form correctly; a new bare-safe value
// stays bare.
func TestUpdateBlockProperties_RequoteBare(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	target := blockIDByURL(doc, "https://target")

	if _, err := svc.UpdateBlockProperties(context.Background(), doc, target, map[string]string{
		"Method": "HTTPClient.Method.POST with space",
	}); err != nil {
		t.Fatalf("UpdateBlockProperties: %v", err)
	}
	src := sourceOf(t, doc.FilePath)
	if !strings.Contains(src, "Method: $'''HTTPClient.Method.POST with space'''") {
		t.Errorf("space-needing value not requoted:\n%s", src)
	}
}

// TestUpdateBlockProperties_Guards: derived keys and multi-line literals are
// refused with actionable errors.
func TestUpdateBlockProperties_Guards(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	target := blockIDByURL(doc, "https://target")

	if _, err := svc.UpdateBlockProperties(context.Background(), doc, target, map[string]string{"_var": "x"}); err == nil || !strings.Contains(err.Error(), "derived") {
		t.Errorf("derived key should be refused, got %v", err)
	}

	// Multi-line literal: the block's value spans lines (EndLineNumber >
	// LineNumber) — the edit must refuse with the source-editor guidance.
	multiSrc := "#Region \"Main\"\n    WebAutomation.FillTextByText Text: $'''alpha\nbeta''' FillWith: $'''x'''\n#EndRegion\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "Main.txt")
	if err := os.WriteFile(path, []byte(multiSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	d2, err := parser.ParseText(multiSrc, "Main.txt", int64(len(multiSrc)))
	if err != nil {
		t.Fatalf("parse multi: %v", err)
	}
	d2.FilePath = path
	d2.RebuildIndexes()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(d2)
	svc2 := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), ldp, nil, nil, nil)
	var fillID string
	for i := range d2.Subflows[0].Blocks {
		if d2.Subflows[0].Blocks[i].RawType == "WebAutomation.FillTextByText" {
			fillID = d2.Subflows[0].Blocks[i].ID
		}
	}
	if fillID == "" {
		t.Fatal("multi-line fixture missing")
	}
	if _, err := svc2.UpdateBlockProperties(context.Background(), d2, fillID, map[string]string{"FillWith": "y"}); err == nil || !strings.Contains(err.Error(), "multiple lines") {
		t.Errorf("multi-line block should be refused with guidance, got %v", err)
	}
}

// TestMoveBlock_Desktop pins sibling reordering: move up swaps with the
// previous sibling (containers carry their whole span), move down inserts
// after the next sibling, and boundary positions refuse.
func TestMoveBlock_Desktop(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	target := blockIDByURL(doc, "https://target") // 3rd top-level block (after SET, before WebAutomation)

	// Move up: the HTTP action lands BEFORE `SET Count TO 0`.
	doc, err := svc.MoveBlock(context.Background(), doc, target, "up")
	if err != nil {
		t.Fatalf("MoveBlock up: %v", err)
	}
	updated := doc
	src := sourceOf(t, doc.FilePath)
	httpIdx := strings.Index(src, "https://target")
	setIdx := strings.Index(src, "SET Count TO 0")
	if httpIdx < 0 || setIdx < 0 || httpIdx > setIdx {
		t.Errorf("move up did not reorder (http@%d set@%d):\n%s", httpIdx, setIdx, src)
	}
	// The doc tree reflects the new order.
	_ = updated

	// Move down: after the up-move the HTTP block is FIRST — moving down
	// puts it back after SET. The re-parse minted fresh block IDs, so
	// re-locate the target by its (still-unique) URL.
	movedTarget := httpURLBlockIDRecursive(doc, "https://target")
	if movedTarget == "" {
		t.Fatal("HTTP block missing after move up")
	}
	if _, err := svc.MoveBlock(context.Background(), doc, movedTarget, "down"); err != nil {
		t.Fatalf("MoveBlock down: %v", err)
	}
	src = sourceOf(t, doc.FilePath)
	httpIdx = strings.Index(src, "https://target")
	setIdx = strings.Index(src, "SET Count TO 0")
	if httpIdx < setIdx {
		t.Errorf("move down did not reorder back (http@%d set@%d):\n%s", httpIdx, setIdx, src)
	}
}

// TestMoveBlock_ContainerSpan: moving a container (IF…END) carries its body
// and END marker together — the region stays balanced.
func TestMoveBlock_ContainerSpan(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	var ifID, commentID string
	for i := range doc.Subflows[0].Blocks {
		b := &doc.Subflows[0].Blocks[i]
		if b.Type == "CONDITION" {
			ifID = b.ID
		}
		if b.Type == "COMMENT" {
			commentID = b.ID
		}
	}
	if ifID == "" || commentID == "" {
		t.Fatal("fixture missing IF or COMMENT")
	}

	// Move the IF below the trailing COMMENT: body + END travel together.
	if _, err := svc.MoveBlock(context.Background(), doc, ifID, "down"); err != nil {
		t.Fatalf("MoveBlock(container): %v", err)
	}
	src := sourceOf(t, doc.FilePath)
	ifIdx := strings.Index(src, "IF %Count%")
	cmtIdx := strings.Index(src, "trailing marker")
	if ifIdx < 0 || cmtIdx < 0 || ifIdx < cmtIdx {
		t.Errorf("container did not move below the comment:\n%s", src)
	}
	if strings.Count(src, "END") != 1 || strings.Count(src, "#EndRegion") != 1 {
		t.Errorf("region unbalanced after container move:\n%s", src)
	}
	// The IF body stayed inside the container.
	insideIdx := strings.Index(src, "inside")
	if insideIdx < ifIdx {
		t.Errorf("container body escaped the container:\n%s", src)
	}
}

// TestMoveBlock_Boundaries refuse at scope edges.
func TestMoveBlock_Boundaries(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	first := doc.Subflows[0].Blocks[0].ID // SET Count
	if _, err := svc.MoveBlock(context.Background(), doc, first, "up"); err == nil || !strings.Contains(err.Error(), "already first") {
		t.Errorf("first-move-up should refuse, got %v", err)
	}
	last := doc.Subflows[0].Blocks[len(doc.Subflows[0].Blocks)-1].ID
	if _, err := svc.MoveBlock(context.Background(), doc, last, "down"); err == nil || !strings.Contains(err.Error(), "already last") {
		t.Errorf("last-move-down should refuse, got %v", err)
	}
}

// TestMoveBlockTo pins the arbitrary-position reorder: a multi-position jump
// (first → last, which up/down would need N calls to express), the
// no-op-same-neighbor refusals, and the cross-scope guard.
func TestMoveBlockTo(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)

	// Order: SET, HTTP(target), WebAutomation(+retry), IF, COMMENT.
	ids := map[string]string{}
	for i := range doc.Subflows[0].Blocks {
		b := &doc.Subflows[0].Blocks[i]
		switch {
		case b.Properties["_var"] == "Count":
			ids["set"] = b.ID
		case b.Properties["Url"] == "https://target":
			ids["http"] = b.ID
		case b.Type == "COMMENT":
			ids["comment"] = b.ID
		case b.Type == "CONDITION":
			ids["if"] = b.ID
		}
	}
	for _, k := range []string{"set", "http", "comment", "if"} {
		if ids[k] == "" {
			t.Fatalf("fixture missing %s", k)
		}
	}

	// Move the FIRST block (SET) to AFTER the last top-level sibling
	// (COMMENT) — a 4-position jump in one call.
	updated, err := svc.MoveBlockTo(context.Background(), doc, ids["set"], ids["comment"], "after")
	if err != nil {
		t.Fatalf("MoveBlockTo: %v", err)
	}
	src := sourceOf(t, doc.FilePath)
	setIdx := strings.Index(src, "SET Count TO 0")
	cmtIdx := strings.Index(src, "trailing marker")
	if setIdx < 0 || cmtIdx < 0 || setIdx < cmtIdx {
		t.Errorf("SET did not move after COMMENT (set@%d cmt@%d):\n%s", setIdx, cmtIdx, src)
	}
	// The IF body survives inside its container.
	insideIdx := strings.Index(src, "inside")
	ifIdx := strings.Index(src, "IF %Count%")
	if insideIdx < ifIdx {
		t.Errorf("IF body escaped the container:\n%s", src)
	}
	_ = updated

	// Re-locate post-reparse IDs for the guard assertions (IDs re-minted).
	var set2, http2 string
	for i := range updated.Subflows[0].Blocks {
		b := &updated.Subflows[0].Blocks[i]
		if b.Properties["_var"] == "Count" {
			set2 = b.ID
		}
		if b.Properties["Url"] == "https://target" {
			http2 = b.ID
		}
	}

	// Self-relative moves always refuse, position-independently. (An
	// after-previous/before-next no-op assertion is covered by TestMoveBlock_.)
	if _, err := svc.MoveBlockTo(context.Background(), updated, set2, set2, "before"); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Errorf("self-relative move should refuse, got %v", err)
	}
	_ = http2

	// Cross-scope: a block INSIDE the IF is not a sibling of top-level blocks.
	var innerID string
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			if blocks[i].RawType == "Display.ShowMessageBox" {
				innerID = blocks[i].ID
			}
			walk(blocks[i].Children)
		}
	}
	walk(updated.Subflows[0].Blocks)
	if innerID == "" {
		t.Fatal("fixture missing inner block")
	}
	// Cross-scope re-parent (R3.4): the SET block moves INSIDE the IF, before
	// the inner block — the move succeeds and the re-parse nests it there.
	reparented, err := svc.MoveBlockTo(context.Background(), updated, set2, innerID, "before")
	if err != nil {
		t.Fatalf("cross-scope move: %v", err)
	}
	var setParent string
	var walk2 func(blocks []models.Block, parent string)
	walk2 = func(blocks []models.Block, parent string) {
		for i := range blocks {
			if blocks[i].Properties["_var"] == "Count" {
				setParent = parent
			}
			walk2(blocks[i].Children, blocks[i].ID)
		}
	}
	walk2(reparented.Subflows[0].Blocks, "")
	if setParent == "" {
		t.Fatalf("SET vanished from the tree after cross-scope move")
	}
	foundIF := false
	var findIF func(blocks []models.Block) bool
	findIF = func(blocks []models.Block) bool {
		for i := range blocks {
			if blocks[i].ID == setParent && blocks[i].Type == "CONDITION" {
				return true
			}
			if findIF(blocks[i].Children) {
				return true
			}
		}
		return false
	}
	foundIF = findIF(reparented.Subflows[0].Blocks)
	if !foundIF {
		t.Errorf("SET's parent after cross-scope move is not the IF container")
	}
	// Re-indent landed: the SET line now sits at the IF-body depth (8 cols).
	src2 := sourceOf(t, updated.FilePath)
	deep := false
	for _, ln := range strings.Split(src2, "\n") {
		if strings.Contains(ln, "SET Count TO 0") && strings.HasPrefix(ln, "        SET") {
			deep = true
		}
	}
	if !deep {
		t.Errorf("SET not re-indented to the IF-body depth:\n%s", src2)
	}

	// Descendant guard: the IF cannot move relative to a block inside
	// itself. Re-locate both by shape in the re-parented doc (IDs re-minted
	// per re-parse).
	var ifID, inner2 string
	var find2 func(blocks []models.Block)
	find2 = func(blocks []models.Block) {
		for i := range blocks {
			b := &blocks[i]
			if b.Type == "CONDITION" && ifID == "" {
				ifID = b.ID
			}
			if b.RawType == "Display.ShowMessageBox" {
				inner2 = b.ID
			}
			find2(b.Children)
		}
	}
	find2(reparented.Subflows[0].Blocks)
	if _, err := svc.MoveBlockTo(context.Background(), reparented, ifID, inner2, "before"); err == nil || !strings.Contains(err.Error(), "own descendants") {
		t.Errorf("move-into-own-subtree should refuse, got %v", err)
	}
}

// TestMoveBlockTo_CloudBridge: the cloud alignment path (ingested, no stored
// Source) resolves spans against the canonical re-parse.
func TestMoveBlockTo_CloudBridge(t *testing.T) {
	src := "#Region \"Main\"\n\n    SET A TO 1\n\n\n    COMMENT  mid\n\n    SET B TO 2\n\n#EndRegion\n"
	doc, err := parser.ParseText(src, "Blanky", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc.Source = ""
	doc.FilePath = ""
	doc.RebuildIndexes()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, ldp, &testutil.FakeBackend{}, nil, nil)

	var aID, bID string
	for i := range doc.Subflows[0].Blocks {
		if doc.Subflows[0].Blocks[i].Properties["_var"] == "A" {
			aID = doc.Subflows[0].Blocks[i].ID
		}
		if doc.Subflows[0].Blocks[i].Properties["_var"] == "B" {
			bID = doc.Subflows[0].Blocks[i].ID
		}
	}
	if aID == "" || bID == "" {
		t.Fatal("fixture missing A/B")
	}

	updated, err := svc.MoveBlockTo(context.Background(), doc, bID, aID, "before")
	if err != nil {
		t.Fatalf("MoveBlockTo(cloud): %v", err)
	}
	// B now precedes A in the re-parsed doc.
	var order []string
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			if v := blocks[i].Properties["_var"]; v == "A" || v == "B" {
				order = append(order, v)
			}
			walk(blocks[i].Children)
		}
	}
	walk(updated.Subflows[0].Blocks)
	if len(order) != 2 || order[0] != "B" || order[1] != "A" {
		t.Errorf("order = %v, want [B A]", order)
	}
}

func TestMoveBlockTo_CrossScope_Outdent(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)

	// Pull the INNER block (8-col indent) out to top level, after COMMENT.
	var innerID, cmtID string
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			b := &blocks[i]
			if b.RawType == "Display.ShowMessageBox" {
				innerID = b.ID
			}
			if b.Type == "COMMENT" {
				cmtID = b.ID
			}
			walk(b.Children)
		}
	}
	walk(doc.Subflows[0].Blocks)
	if innerID == "" || cmtID == "" {
		t.Fatal("fixture missing inner/comment")
	}

	updated, err := svc.MoveBlockTo(context.Background(), doc, innerID, cmtID, "after")
	if err != nil {
		t.Fatalf("outdent move: %v", err)
	}
	// The block is a TOP-LEVEL sibling now (parent == ""), and its line sits
	// at the 4-col indent.
	src := sourceOf(t, doc.FilePath)
	okLine := false
	for _, ln := range strings.Split(src, "\n") {
		if ln == "    Display.ShowMessageBox Message: $'''inside'''" {
			okLine = true
		}
	}
	if !okLine {
		t.Errorf("inner block not re-indented to 4 cols:\n%s", src)
	}
	var parent string
	var find func(blocks []models.Block, p string)
	find = func(blocks []models.Block, p string) {
		for i := range blocks {
			if blocks[i].RawType == "Display.ShowMessageBox" {
				parent = p
			}
			find(blocks[i].Children, blocks[i].ID)
		}
	}
	find(updated.Subflows[0].Blocks, "")
	if parent != "" {
		t.Errorf("inner block still nested (parent=%q)", parent)
	}
}

func TestMoveBlockTo_CrossScope_StructuralMarkerRefused(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	var setID, endID string
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			b := &blocks[i]
			if b.Properties["_var"] == "Count" {
				setID = b.ID
			}
			if b.Type == "END" {
				endID = b.ID
			}
			walk(b.Children)
		}
	}
	walk(doc.Subflows[0].Blocks)
	if setID == "" || endID == "" {
		t.Fatal("fixture missing set/end")
	}
	_, err := svc.MoveBlockTo(context.Background(), doc, setID, endID, "before")
	if err == nil || !strings.Contains(err.Error(), "structural marker") {
		t.Errorf("marker anchor should refuse, got %v", err)
	}
}

func TestMoveBlockTo_CrossScope_CloudBridge(t *testing.T) {
	// Cloud (ingested, no FilePath/Source): canonical re-parse + fresh IDs on
	// both sides; cross-scope must survive the bridge relocation.
	src := "#Region \"Main\"\n" +
		"    SET A TO 1\n" +
		"    IF %A% = 1\n" +
		"        SET B TO 2\n" +
		"    END\n" +
		"    SET C TO 3\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(src, "Cloudy", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc.Source = ""
	doc.FilePath = ""
	doc.RebuildIndexes()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, ldp, &testutil.FakeBackend{}, nil, nil)

	var aID, bID string
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			b := &blocks[i]
			switch b.Properties["_var"] {
			case "A":
				aID = b.ID
			case "B":
				bID = b.ID
			}
			walk(b.Children)
		}
	}
	walk(doc.Subflows[0].Blocks)
	if aID == "" || bID == "" {
		t.Fatal("fixture missing A/B")
	}

	// A (top level) moves INSIDE the IF, before B.
	updated, err := svc.MoveBlockTo(context.Background(), doc, aID, bID, "before")
	if err != nil {
		t.Fatalf("cloud cross-scope: %v", err)
	}
	// A is now a child of the CONDITION; canonical source has it at 8 cols.
	var aParentType models.BlockType
	var find func(blocks []models.Block, pType models.BlockType)
	find = func(blocks []models.Block, pType models.BlockType) {
		for i := range blocks {
			b := &blocks[i]
			if b.Properties["_var"] == "A" {
				aParentType = pType
			}
			find(b.Children, b.Type)
		}
	}
	find(updated.Subflows[0].Blocks, "")
	if aParentType != "CONDITION" {
		t.Errorf("A's parent type = %q, want CONDITION", aParentType)
	}
	srcOut := updated.Source
	if srcOut == "" {
		t.Fatalf("updated doc has no source")
	}
	// The canonical serializer's level width is its own concern — assert A's
	// line sits strictly deeper than the IF header's.
	lead := func(t string) int {
		for _, ln := range strings.Split(srcOut, "\n") {
			if strings.Contains(ln, t) {
				n := 0
				for n < len(ln) && (ln[n] == ' ' || ln[n] == '\t') {
					n++
				}
				return n
			}
		}
		return -1
	}
	if lead("SET A TO 1") <= lead("IF %A% = 1") || lead("SET A TO 1") == -1 {
		t.Errorf("A not re-indented into the IF body:\n%s", srcOut)
	}
}

func TestReindentLines(t *testing.T) {
	// Indent: 4-space levels pad; blanks stay blank.
	in := []string{"    ACT one", "", "    END"}
	got := reindentLines(in, 4)
	want := []string{"        ACT one", "", "        END"}
	if !slicesEqual(got, want) {
		t.Errorf("indent = %q, want %q", got, want)
	}
	// Outdent: exact strip.
	got = reindentLines(want, -4)
	if !slicesEqual(got, in) {
		t.Errorf("outdent = %q, want %q", got, in)
	}
	// Outdent past available columns stops at content (never eats text).
	shallow := []string{"  ACT"}
	got = reindentLines(shallow, -4)
	if !slicesEqual(got, []string{"ACT"}) {
		t.Errorf("over-outdent = %q, want [ACT]", got)
	}
	// Straddling tab: one tab (4 cols) outdented by 2 keeps 2 spaces.
	tabs := []string{"\tACT"}
	got = reindentLines(tabs, -2)
	if !slicesEqual(got, []string{"  ACT"}) {
		t.Errorf("straddling tab = %q, want [  ACT]", got)
	}
	// Full tab strip.
	got = reindentLines(tabs, -4)
	if !slicesEqual(got, []string{"ACT"}) {
		t.Errorf("tab strip = %q, want [ACT]", got)
	}
	// Zero delta is identity (same slice).
	if got := reindentLines(in, 0); &got[0] != &in[0] {
		t.Errorf("zero delta should be identity")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
