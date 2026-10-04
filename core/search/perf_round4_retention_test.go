package search

import (
	"fmt"
	"runtime"
	"testing"
	"time"
	"weak"

	"pad-core/models"
)

// collectedWithin reports whether gone turns true within a few forced GC
// cycles — the proof that an object is unreachable, not merely unused.
func collectedWithin(gone func() bool) bool {
	for i := 0; i < 10; i++ {
		runtime.GC()
		if gone() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// syntheticDoc builds a flow of exactly n blocks in one subflow — every tenth
// top-level block nests a child, and the last block is always a top-level one
// — with the parser's lookup maps built, so it looks like a parsed document to
// the index and to Search.
func syntheticDoc(n int) *models.FlowDocument {
	doc := &models.FlowDocument{ID: "retention", Name: "Retention", Subflows: []models.Subflow{{ID: "sf-main", Name: "Main"}}}
	sf := &doc.Subflows[0]
	total := 0
	for total < n {
		i := len(sf.Blocks)
		b := models.Block{
			ID: fmt.Sprintf("b%d", i), Name: fmt.Sprintf("Click on element %d", i), Type: models.BlockTypeAction,
			RawType:    "WebAutomation.Click.Click",
			Properties: map[string]string{"Selector": fmt.Sprintf("Submit button %d", i)},
			Variables:  []string{"WebPage"},
			SubflowID:  sf.ID,
		}
		total++
		if i%10 == 0 && total < n-1 {
			b.Children = []models.Block{{
				ID: fmt.Sprintf("b%d-child", i), Name: fmt.Sprintf("Set variable %d", i), Type: models.BlockTypeVariable,
				RawType: "Variables.SetVariable", Variables: []string{"Counter"}, SubflowID: sf.ID,
			}}
			total++
		}
		sf.Blocks = append(sf.Blocks, b)
	}
	doc.RebuildIndexes()
	return doc
}

// buildAndDrop builds an index over a fresh document and returns it with only
// WEAK references to the document and to its block tree: once this returns,
// nothing but the index could keep either alive.
func buildAndDrop(n int) (*SearchIndex, weak.Pointer[models.FlowDocument], weak.Pointer[models.Block]) {
	doc := syntheticDoc(n)
	idx := NewSearchIndex(doc.ID, doc)
	return idx, weak.Make(doc), weak.Make(&doc.Subflows[0].Blocks[0])
}

// TestSearchIndex_DoesNotPinTheDocument is the memory contract behind
// FlowService's index cache: the cache keeps the index strongly and the
// document only weakly, so a superseded document (a new version of the flow,
// a flow evicted from the document cache) must become garbage while its index
// is still cached. The index used to hold a *models.Block per block — every
// block of every subflow — which kept the whole block tree (~100 MiB for a
// 10 MiB flow) alive for as long as the index was cached, and the weak
// pointer released nothing.
func TestSearchIndex_DoesNotPinTheDocument(t *testing.T) {
	idx, doc, blocks := buildAndDrop(5000)
	if got := idx.BlockCount(); got != 5000 {
		t.Fatalf("BlockCount = %d, want 5000", got)
	}
	if !collectedWithin(func() bool { return doc.Value() == nil }) {
		t.Error("the document is still reachable while its search index is alive: the index pins it, so a weakly-held document can never be released")
	}
	if !collectedWithin(func() bool { return blocks.Value() == nil }) {
		t.Error("the block tree is still reachable while its search index is alive: the index pins the subflows' block slices")
	}
	// The index outlives its document intact: tokens are the ids' own
	// strings, not references into the document. Every top-level block is a
	// "Click ..." action; the nested ones are "Set variable" blocks.
	clicks, sets := len(idx.tokens["click"]), len(idx.tokens["counter"])
	if clicks == 0 || sets == 0 || clicks+sets != 5000 {
		t.Errorf("tokens list %d click + %d counter blocks after the document was collected, want 5000 in total", clicks, sets)
	}
	runtime.KeepAlive(idx)
}
