package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// The round-trip gates in autofix_test.go all target TOP-LEVEL blocks, where
// raw-column Indent (0) and level-count Indent (0) coincide — which is how
// the fixers' level-multiplier bug (strings.Repeat("    ", block.Indent) on a
// raw column count) survived them. These gates target blocks NESTED one or
// more levels deep, where the two interpretations diverge by 4x, and lock the
// column-faithful semantics of indentCols/indentColsDeeper.

// findBlockByRaw returns the first block whose RawType has the given prefix.
func findBlockByRaw(t *testing.T, doc *models.FlowDocument, prefix string) *models.Block {
	t.Helper()
	for _, b := range doc.BlocksByID {
		if strings.HasPrefix(b.RawType, prefix) {
			return b
		}
	}
	t.Fatalf("no block with RawType prefix %q", prefix)
	return nil
}

// TestFixerIndentation_WrapErrorHandlerNestsCorrectly: wrapping a block that
// is nested inside a LOOP must produce a handler at the block's own column
// whose body nests INSIDE it. Pre-fix, the header/footer were emitted at
// 4× the block's column (a level-1 block got 16 spaces), the parser re-nested
// the handler as EMPTY, and the fix loop livelocked.
func TestFixerIndentation_WrapErrorHandlerNestsCorrectly(t *testing.T) {
	const source = `#Region "Main"
LOOP WHILE %RetryCount% < 3
    HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x' => R
    SET RetryCount TO %RetryCount% + 1
END
#EndRegion`

	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	block := findBlockByRaw(t, doc, "HTTPClient")
	if block.Indent == 0 {
		t.Fatalf("fixture sanity: HTTP block must be nested (Indent=%d)", block.Indent)
	}

	patched := ApplyPatch(source, WrapInErrorHandlerPatch(block))
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, patched)
	}

	// The HTTP action must now have an error-handler ANCESTOR (nested inside
	// the wrap) — the pre-fix mis-indentation left the handler empty. (The
	// classifier canonicalizes the source `ON BLOCK ERROR` to RawType
	// "OnBlockError".)
	http2 := findBlockByRaw(t, doc2, "HTTPClient")
	for _, parentID := range ancestorsOf(doc2, http2) {
		if p := doc2.BlocksByID[parentID]; p != nil && p.RawType == "OnBlockError" {
			return // correctly nested
		}
	}
	t.Errorf("HTTP action is not inside the error handler after wrap (mis-nested):\n%s", patched)
}

// ancestorsOf walks the parent chain via each subflow's block tree.
func ancestorsOf(doc *models.FlowDocument, target *models.Block) []string {
	var chain []string
	for _, sf := range doc.Subflows {
		chain = append(chain, ancestorsIn(&sf, target)...)
	}
	return chain
}

func ancestorsIn(sf *models.Subflow, target *models.Block) []string {
	var walk func(b *models.Block, chain []string) []string
	walk = func(b *models.Block, chain []string) []string {
		for i := range b.Children {
			next := append(chain, b.ID) //nolint:gocritic // small trees
			if b.Children[i].ID == target.ID {
				return next
			}
			if got := walk(&b.Children[i], next); got != nil {
				return got
			}
		}
		return nil
	}
	for i := range sf.Blocks {
		if sf.Blocks[i].ID == target.ID {
			return nil // top-level: no ancestors
		}
		if got := walk(&sf.Blocks[i], nil); got != nil {
			return got
		}
	}
	return nil
}

// TestFixerIndentation_InsertHandlerLogNestsCorrectly: inserting the handler
// log line must place it INSIDE the handler (handler column + one level).
// Pre-fix it landed at 4×(column+1) — past the END, never nested, growing
// every iteration.
func TestFixerIndentation_InsertHandlerLogNestsCorrectly(t *testing.T) {
	const source = `#Region "Main"
WebAutomation.OpenBrowser Chrome: Chrome URL: '''https://example.com'''
ON BLOCK ERROR
END
Display.CloseBrowser
#EndRegion`

	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	handler := findBlockByRaw(t, doc, "OnBlockError")
	if handler.RawType != "OnBlockError" {
		t.Fatalf("fixture sanity: expected classifier canonical OnBlockError, got %q", handler.RawType)
	}
	if handler.Indent != 0 {
		t.Fatalf("fixture sanity: handler at top level, Indent=%d", handler.Indent)
	}

	patched := ApplyPatch(source, InsertHandlerLogPatch(handler))
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, patched)
	}
	handler2 := findBlockByRaw(t, doc2, "OnBlockError")
	hasChild := false
	for i := range handler2.Children {
		if strings.HasPrefix(handler2.Children[i].RawType, "Display.ShowMessageBox") {
			hasChild = true
		}
	}
	if !hasChild {
		t.Errorf("log line did not nest inside the handler:\n%s", patched)
	}
}

// TestFixerIndentation_TwoSpaceConvention: computeIndent counts raw columns
// (space=1), so a 2-space-indented file has level-1 blocks with Indent=2.
// Column-faithful indentation must still nest (+4 columns is deeper than any
// parent), where level math (2/4=0 levels) would flatten to column 0.
func TestFixerIndentation_TwoSpaceConvention(t *testing.T) {
	const source = `#Region "Main"
LOOP WHILE %RetryCount% < 3
  HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x' => R
END
#EndRegion`

	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	block := findBlockByRaw(t, doc, "HTTPClient")

	patched := ApplyPatch(source, WrapInErrorHandlerPatch(block))
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, patched)
	}
	http2 := findBlockByRaw(t, doc2, "HTTPClient")
	for _, parentID := range ancestorsOf(doc2, http2) {
		if p := doc2.BlocksByID[parentID]; p != nil && p.RawType == "OnBlockError" {
			return // correctly nested even in a 2-space file
		}
	}
	t.Errorf("HTTP action not inside handler in 2-space file:\n%s", patched)
}

// TestFixerIndentation_WrapRetryKeepsLoopBodyNested: wrap-in-retry around a
// nested action must keep the action nested inside the generated LOOP (the
// multi-line header/footer use the same column math).
func TestFixerIndentation_WrapRetryKeepsLoopBodyNested(t *testing.T) {
	const source = `#Region "Main"
IF %Ok% THEN
    WebAutomation.ClickElement Element: 'btn' Instance: %Browser%
END
#EndRegion`

	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	block := findBlockByRaw(t, doc, "WebAutomation.ClickElement")
	if block.Indent == 0 {
		t.Fatalf("fixture sanity: click block must be nested (Indent=%d)", block.Indent)
	}

	patched := ApplyPatch(source, WrapInRetryPatch(block))
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, patched)
	}
	click2 := findBlockByRaw(t, doc2, "WebAutomation.ClickElement")
	sawLoop, sawIf := false, false
	for _, parentID := range ancestorsOf(doc2, click2) {
		if p := doc2.BlocksByID[parentID]; p != nil {
			if p.RawType == "LOOP" {
				sawLoop = true
			}
			if p.RawType == "IF" {
				sawIf = true
			}
		}
	}
	if !sawLoop || !sawIf {
		t.Errorf("after retry wrap: click inside LOOP=%v inside IF=%v (want both):\n%s", sawLoop, sawIf, patched)
	}
}
