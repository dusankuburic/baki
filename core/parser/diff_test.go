package parser

import (
	"testing"

	"pad-core/models"
)

// ---- helpers ----------------------------------------------------------------

func makeBlock(rawType, name string, indent int, children ...models.Block) models.Block {
	return models.Block{
		ID:       rawType + ":" + name,
		RawType:  rawType,
		Name:     name,
		Indent:   indent,
		Children: children,
	}
}

func makeSubflow(name string, blocks ...models.Block) models.Subflow {
	return models.Subflow{ID: "sf-" + name, Name: name, Blocks: blocks}
}

func makeDoc(id string, subflows ...models.Subflow) *models.FlowDocument {
	doc := &models.FlowDocument{ID: id, Subflows: subflows}
	return doc
}

// ---- blocksEqual ------------------------------------------------------------

func TestBlocksEqual_Same(t *testing.T) {
	a := makeBlock("ACTION", "Click", 0)
	b := makeBlock("ACTION", "Click", 0)
	if !blocksEqual(&a, &b) {
		t.Error("expected equal blocks to be equal")
	}
}

func TestBlocksEqual_DifferentRawType(t *testing.T) {
	a := makeBlock("ACTION", "Click", 0)
	b := makeBlock("LOOP", "Click", 0)
	if blocksEqual(&a, &b) {
		t.Error("different RawType should not be equal")
	}
}

func TestBlocksEqual_DifferentName(t *testing.T) {
	a := makeBlock("ACTION", "Click", 0)
	b := makeBlock("ACTION", "Submit", 0)
	if blocksEqual(&a, &b) {
		t.Error("different Name should not be equal")
	}
}

func TestBlocksEqual_DifferentIndent(t *testing.T) {
	a := makeBlock("ACTION", "Click", 0)
	b := makeBlock("ACTION", "Click", 1)
	if blocksEqual(&a, &b) {
		t.Error("different Indent should not be equal")
	}
}

func TestBlocksEqual_DifferentPropertyValue(t *testing.T) {
	// Same RawType/Name/Indent but a changed property value (e.g. a Url:) must
	// NOT compare equal — otherwise a config-only edit is invisible in the diff.
	a := models.Block{RawType: "ACTION", Name: "Navigate", Indent: 0,
		Properties: map[string]string{"Url": "https://a.example"}}
	b := models.Block{RawType: "ACTION", Name: "Navigate", Indent: 0,
		Properties: map[string]string{"Url": "https://b.example"}}
	if blocksEqual(&a, &b) {
		t.Error("blocks differing only in a property value should not be equal")
	}
}

func TestBlocksEqual_SameProperties(t *testing.T) {
	a := models.Block{RawType: "ACTION", Name: "Navigate", Indent: 0,
		Properties: map[string]string{"Url": "https://a.example", "Timeout": "30"}}
	b := models.Block{RawType: "ACTION", Name: "Navigate", Indent: 0,
		Properties: map[string]string{"Timeout": "30", "Url": "https://a.example"}}
	if !blocksEqual(&a, &b) {
		t.Error("blocks with identical properties (any order) should be equal")
	}
}

// ---- DiffFlows: property-only edit surfaces as a change ---------------------

func TestDiffFlows_PropertyOnlyEdit(t *testing.T) {
	// A single block whose only change is a property value must produce a
	// visible diff (remove-old + add-new), not an all-ChangeNone result.
	oldBlk := models.Block{RawType: "ACTION", Name: "Navigate", Indent: 0,
		Properties: map[string]string{"Url": "https://old.example"}}
	newBlk := models.Block{RawType: "ACTION", Name: "Navigate", Indent: 0,
		Properties: map[string]string{"Url": "https://new.example"}}

	sfOld := makeSubflow("Main", oldBlk)
	sfNew := makeSubflow("Main", newBlk)

	diff := DiffFlows(makeDoc("old", sfOld), makeDoc("new", sfNew))

	if len(diff.Subflows) != 1 {
		t.Fatalf("expected 1 subflow diff, got %d", len(diff.Subflows))
	}
	sdiff := diff.Subflows[0]
	if sdiff.Change != models.ChangeModified {
		t.Errorf("property-only edit: subflow change = %q, want %q", sdiff.Change, models.ChangeModified)
	}
	var added, removed int
	for _, bd := range sdiff.Blocks {
		switch bd.Change {
		case models.ChangeAdded:
			added++
		case models.ChangeRemoved:
			removed++
		}
	}
	if added != 1 || removed != 1 {
		t.Errorf("property-only edit should surface as 1 add + 1 remove, got %d add / %d remove", added, removed)
	}
}

// ---- max (local) ------------------------------------------------------------

func TestMax(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{3, 5, 5},
		{5, 3, 5},
		{4, 4, 4},
		{0, -1, 0},
	}
	for _, tc := range cases {
		if got := max(tc.a, tc.b); got != tc.want {
			t.Errorf("max(%d,%d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// ---- flattenBlocks ----------------------------------------------------------

func TestFlattenBlocks_Flat(t *testing.T) {
	blocks := []models.Block{
		makeBlock("A", "1", 0),
		makeBlock("A", "2", 0),
	}
	got := flattenBlocks(blocks)
	if len(got) != 2 {
		t.Errorf("expected 2 flat blocks, got %d", len(got))
	}
}

func TestFlattenBlocks_Nested(t *testing.T) {
	inner := makeBlock("ACTION", "inner", 1)
	outer := makeBlock("LOOP", "outer", 0, inner)
	got := flattenBlocks([]models.Block{outer})
	// outer + inner = 2
	if len(got) != 2 {
		t.Errorf("expected 2 blocks (outer + inner), got %d", len(got))
	}
	if got[0].Name != "outer" {
		t.Errorf("first block should be outer, got %q", got[0].Name)
	}
	if got[1].Name != "inner" {
		t.Errorf("second block should be inner, got %q", got[1].Name)
	}
}

func TestFlattenBlocks_Empty(t *testing.T) {
	if got := flattenBlocks(nil); len(got) != 0 {
		t.Errorf("expected 0 for nil input, got %d", len(got))
	}
}

// ---- wrapBlocksAsDiff -------------------------------------------------------

func TestWrapBlocksAsDiff_Added(t *testing.T) {
	blocks := []models.Block{makeBlock("ACTION", "a", 0)}
	diffs := wrapBlocksAsDiff(blocks, models.ChangeAdded)
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
	if diffs[0].Change != models.ChangeAdded {
		t.Errorf("change = %q, want %q", diffs[0].Change, models.ChangeAdded)
	}
	if diffs[0].New == nil {
		t.Error("added block diff should have non-nil New")
	}
	if diffs[0].Old != nil {
		t.Error("added block diff should have nil Old")
	}
}

func TestWrapBlocksAsDiff_Removed(t *testing.T) {
	blocks := []models.Block{makeBlock("ACTION", "a", 0)}
	diffs := wrapBlocksAsDiff(blocks, models.ChangeRemoved)
	if diffs[0].Change != models.ChangeRemoved {
		t.Errorf("change = %q, want %q", diffs[0].Change, models.ChangeRemoved)
	}
	if diffs[0].Old == nil {
		t.Error("removed block diff should have non-nil Old")
	}
	if diffs[0].New != nil {
		t.Error("removed block diff should have nil New")
	}
}

func TestWrapBlocksAsDiff_WithChildren(t *testing.T) {
	child := makeBlock("ACTION", "child", 1)
	parent := makeBlock("LOOP", "parent", 0, child)
	diffs := wrapBlocksAsDiff([]models.Block{parent}, models.ChangeAdded)
	// Parent + child = 2 diffs (recursive wrapping).
	if len(diffs) != 2 {
		t.Errorf("expected 2 diffs (parent + child), got %d", len(diffs))
	}
}

// ---- DiffFlows: identical documents ----------------------------------------

func TestDiffFlows_Identical(t *testing.T) {
	sf := makeSubflow("Main",
		makeBlock("ACTION", "Click", 0),
		makeBlock("ACTION", "Type", 0),
	)
	old := makeDoc("doc-1", sf)
	new := makeDoc("doc-2", sf)

	diff := DiffFlows(old, new)

	if diff.OldID != "doc-1" || diff.NewID != "doc-2" {
		t.Errorf("IDs wrong: old=%q new=%q", diff.OldID, diff.NewID)
	}
	if len(diff.Subflows) != 1 {
		t.Fatalf("expected 1 subflow diff, got %d", len(diff.Subflows))
	}
	sdiff := diff.Subflows[0]
	if sdiff.Change != models.ChangeNone {
		t.Errorf("identical subflow should have ChangeNone, got %q", sdiff.Change)
	}
	for _, bd := range sdiff.Blocks {
		if bd.Change != models.ChangeNone {
			t.Errorf("block %q should have ChangeNone, got %q", bd.New.Name, bd.Change)
		}
	}
}

// ---- DiffFlows: subflow added -----------------------------------------------

func TestDiffFlows_AddedSubflow(t *testing.T) {
	sfOld := makeSubflow("Main", makeBlock("ACTION", "Click", 0))
	sfNew := makeSubflow("Main", makeBlock("ACTION", "Click", 0))
	sfAdded := makeSubflow("Helper", makeBlock("ACTION", "Submit", 0))

	old := makeDoc("old", sfOld)
	new := makeDoc("new", sfNew, sfAdded)

	diff := DiffFlows(old, new)

	var found bool
	for _, sd := range diff.Subflows {
		if sd.Name == "Helper" {
			found = true
			if sd.Change != models.ChangeAdded {
				t.Errorf("Helper subflow change = %q, want %q", sd.Change, models.ChangeAdded)
			}
			if len(sd.Blocks) == 0 {
				t.Error("added subflow should list its blocks as diffs")
			}
		}
	}
	if !found {
		t.Error("Helper subflow not found in diff output")
	}
}

// ---- DiffFlows: subflow removed ---------------------------------------------

func TestDiffFlows_RemovedSubflow(t *testing.T) {
	sfMain := makeSubflow("Main", makeBlock("ACTION", "Click", 0))
	sfRemoved := makeSubflow("OldHelper", makeBlock("ACTION", "Submit", 0))

	old := makeDoc("old", sfMain, sfRemoved)
	new := makeDoc("new", sfMain)

	diff := DiffFlows(old, new)

	var found bool
	for _, sd := range diff.Subflows {
		if sd.Name == "OldHelper" {
			found = true
			if sd.Change != models.ChangeRemoved {
				t.Errorf("OldHelper change = %q, want %q", sd.Change, models.ChangeRemoved)
			}
		}
	}
	if !found {
		t.Error("removed subflow OldHelper not found in diff output")
	}
}

// ---- DiffFlows: block added within common subflow ---------------------------

func TestDiffFlows_BlockAdded(t *testing.T) {
	sfOld := makeSubflow("Main", makeBlock("ACTION", "Click", 0))
	sfNew := makeSubflow("Main",
		makeBlock("ACTION", "Click", 0),
		makeBlock("ACTION", "NewStep", 0), // added
	)

	diff := DiffFlows(makeDoc("old", sfOld), makeDoc("new", sfNew))

	if len(diff.Subflows) != 1 {
		t.Fatalf("expected 1 subflow diff, got %d", len(diff.Subflows))
	}
	sdiff := diff.Subflows[0]
	if sdiff.Change != models.ChangeModified {
		t.Errorf("subflow change = %q, want %q", sdiff.Change, models.ChangeModified)
	}
	var addedCount int
	for _, bd := range sdiff.Blocks {
		if bd.Change == models.ChangeAdded {
			addedCount++
		}
	}
	if addedCount != 1 {
		t.Errorf("expected 1 added block, got %d", addedCount)
	}
}

// ---- DiffFlows: block removed within common subflow -------------------------

func TestDiffFlows_BlockRemoved(t *testing.T) {
	sfOld := makeSubflow("Main",
		makeBlock("ACTION", "Click", 0),
		makeBlock("ACTION", "OldStep", 0), // will be removed
	)
	sfNew := makeSubflow("Main", makeBlock("ACTION", "Click", 0))

	diff := DiffFlows(makeDoc("old", sfOld), makeDoc("new", sfNew))

	sdiff := diff.Subflows[0]
	var removedCount int
	for _, bd := range sdiff.Blocks {
		if bd.Change == models.ChangeRemoved {
			removedCount++
		}
	}
	if removedCount != 1 {
		t.Errorf("expected 1 removed block, got %d", removedCount)
	}
}

// ---- DiffFlows: empty documents ---------------------------------------------

func TestDiffFlows_BothEmpty(t *testing.T) {
	diff := DiffFlows(makeDoc("a"), makeDoc("b"))
	if len(diff.Subflows) != 0 {
		t.Errorf("expected 0 subflow diffs for empty docs, got %d", len(diff.Subflows))
	}
}

// ---- lcs and backtrack: tested through diffSubflow --------------------------

func TestDiffSubflow_AllUnchanged(t *testing.T) {
	blocks := []models.Block{
		makeBlock("ACTION", "Step1", 0),
		makeBlock("ACTION", "Step2", 0),
	}
	sf := makeSubflow("Main", blocks...)
	result := diffSubflow(&sf, &sf)

	if result.Change != models.ChangeNone {
		t.Errorf("identical subflow: Change = %q, want %q", result.Change, models.ChangeNone)
	}
	for _, bd := range result.Blocks {
		if bd.Change != models.ChangeNone {
			t.Errorf("block should be ChangeNone, got %q", bd.Change)
		}
	}
}

func TestDiffSubflow_LCS_PreservesCommonBlocks(t *testing.T) {
	// Old: A B C  →  New: A X C  (B replaced by X via remove+add)
	sfOld := makeSubflow("Main",
		makeBlock("ACTION", "A", 0),
		makeBlock("ACTION", "B", 0),
		makeBlock("ACTION", "C", 0),
	)
	sfNew := makeSubflow("Main",
		makeBlock("ACTION", "A", 0),
		makeBlock("ACTION", "X", 0),
		makeBlock("ACTION", "C", 0),
	)
	result := diffSubflow(&sfOld, &sfNew)

	// A and C are in LCS → ChangeNone; B is removed, X is added.
	unchanged := 0
	added := 0
	removed := 0
	for _, bd := range result.Blocks {
		switch bd.Change {
		case models.ChangeNone:
			unchanged++
		case models.ChangeAdded:
			added++
		case models.ChangeRemoved:
			removed++
		}
	}
	if unchanged != 2 {
		t.Errorf("expected 2 unchanged (A, C), got %d", unchanged)
	}
	if added != 1 {
		t.Errorf("expected 1 added (X), got %d", added)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed (B), got %d", removed)
	}
}
