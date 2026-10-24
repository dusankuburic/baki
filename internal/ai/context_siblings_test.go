package ai

import (
	"fmt"
	"strings"
	"testing"

	"pad-core/models"
)

// surroundingSection returns the "Surrounding Blocks" section of a built context ("" when absent).
func surroundingSection(ctx string) string {
	i := strings.Index(ctx, "## Surrounding Blocks")
	if i < 0 {
		return ""
	}
	rest := ctx[i:]
	if j := strings.Index(rest[1:], "\n## "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

// TestBuildContext_SurroundingBlocksAreTheNeighbours pins R-analyzer-boundary-1: the window is the ±3 blocks
// around the selected one, with the selection marked — not the parent's first three children whatever the
// selection, which is what the model was told for every block past the fourth.
func TestBuildContext_SurroundingBlocksAreTheNeighbours(t *testing.T) {
	blocks := make([]models.Block, 20)
	for i := range blocks {
		blocks[i] = models.Block{
			ID: fmt.Sprintf("b%d", i), Name: fmt.Sprintf("Step %d", i), Type: models.BlockTypeAction,
			SubflowID: "sf1", LineNumber: i + 1,
		}
	}
	doc := &models.FlowDocument{ID: "f", Name: "Flow", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}}}
	doc.RebuildIndexes()
	selected := doc.BlocksByID["b10"]

	_, ctx := BuildContext(ContextRequest{Flow: doc, SelectedBlock: selected, SelectedSubflow: doc.SubflowsByID["sf1"],
		TokenBudget: 8000})
	section := surroundingSection(ctx)
	if section == "" {
		t.Fatalf("no Surrounding Blocks section:\n%s", ctx)
	}
	for i := 7; i <= 13; i++ {
		if !strings.Contains(section, fmt.Sprintf("Step %d", i)) {
			t.Errorf("neighbour Step %d missing from the window:\n%s", i, section)
		}
	}
	for _, far := range []int{0, 1, 2, 6, 14, 19} {
		if strings.Contains(section, fmt.Sprintf("Step %d\n", far)) || strings.Contains(section, fmt.Sprintf("Step %d ", far)) {
			t.Errorf("Step %d is outside the ±3 window but was listed:\n%s", far, section)
		}
	}
	if !strings.Contains(section, "Step 10 ← selected") {
		t.Errorf("the selected block is not marked:\n%s", section)
	}
}

// TestBuildContext_SurroundingBlocksOfANestedBlock: the window is taken from the parent's children.
func TestBuildContext_SurroundingBlocksOfANestedBlock(t *testing.T) {
	children := make([]models.Block, 9)
	for i := range children {
		children[i] = models.Block{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("Inner %d", i), Type: models.BlockTypeAction,
			SubflowID: "sf1", ParentID: "loop", LineNumber: 10 + i}
	}
	loop := models.Block{ID: "loop", Name: "Loop", Type: models.BlockTypeLoop, SubflowID: "sf1", LineNumber: 9, Children: children}
	doc := &models.FlowDocument{ID: "f", Name: "Flow", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{loop}}}}
	doc.RebuildIndexes()

	_, ctx := BuildContext(ContextRequest{Flow: doc, SelectedBlock: doc.BlocksByID["c6"], SelectedSubflow: doc.SubflowsByID["sf1"],
		TokenBudget: 8000})
	section := surroundingSection(ctx)
	for i := 3; i <= 8; i++ {
		if !strings.Contains(section, fmt.Sprintf("Inner %d", i)) {
			t.Errorf("neighbour Inner %d missing:\n%s", i, section)
		}
	}
	if strings.Contains(section, "Inner 2") {
		t.Errorf("Inner 2 is outside the window:\n%s", section)
	}
	if !strings.Contains(section, "Inner 6 ← selected") {
		t.Errorf("the selected block is not marked:\n%s", section)
	}
}

// TestBuildContext_NoSurroundingBlocksForAnOnlyChild: a block with no siblings gets no section.
func TestBuildContext_NoSurroundingBlocksForAnOnlyChild(t *testing.T) {
	doc := &models.FlowDocument{ID: "f", Name: "Flow", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{
		{ID: "only", Name: "Only", Type: models.BlockTypeAction, SubflowID: "sf1", LineNumber: 1},
	}}}}
	doc.RebuildIndexes()
	_, ctx := BuildContext(ContextRequest{Flow: doc, SelectedBlock: doc.BlocksByID["only"], SelectedSubflow: doc.SubflowsByID["sf1"],
		TokenBudget: 8000})
	if s := surroundingSection(ctx); s != "" {
		t.Errorf("an only child got a Surrounding Blocks section:\n%s", s)
	}
}
