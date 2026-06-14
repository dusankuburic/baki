package analyzer

import (
	"testing"

	"pad-core/models"
)

func fcDoc(id string, subflows ...models.Subflow) *models.FlowDocument {
	return &models.FlowDocument{ID: id, Subflows: subflows}
}

func fcSub(id, name string, blocks ...models.Block) models.Subflow {
	return models.Subflow{ID: id, Name: name, Blocks: blocks}
}

func fcBlock(id, name string, btype models.BlockType, rawType string, props map[string]string) models.Block {
	return models.Block{ID: id, Name: name, Type: btype, RawType: rawType, Properties: props}
}

func TestCompareFlows_IdenticalFlows(t *testing.T) {
	sf := fcSub("sf-1", "Main",
		fcBlock("b-1", "Action1", models.BlockTypeAction, "HTTP", map[string]string{"url": "https://api.example.com"}),
		fcBlock("b-2", "Action2", models.BlockTypeAction, "SQL", map[string]string{"table": "users"}),
	)
	docA := fcDoc("flow-a", sf)
	docB := fcDoc("flow-b", sf)

	result := CompareFlows(docA, docB)

	if result.SharedBlocks != 2 {
		t.Errorf("expected 2 shared blocks, got %d", result.SharedBlocks)
	}
	if result.AddedBlocks != 0 || result.RemovedBlocks != 0 {
		t.Errorf("expected 0 added/removed, got added=%d removed=%d", result.AddedBlocks, result.RemovedBlocks)
	}
	if result.Similarity != 1.0 {
		t.Errorf("expected similarity 1.0, got %f", result.Similarity)
	}
}

func TestCompareFlows_CompletelyDifferent(t *testing.T) {
	docA := fcDoc("flow-a",
		fcSub("sf-1", "Main",
			fcBlock("b-1", "ActionA", models.BlockTypeAction, "HTTP", nil),
		),
	)
	docB := fcDoc("flow-b",
		fcSub("sf-2", "Main",
			fcBlock("b-2", "ActionB", models.BlockTypeAction, "SQL", nil),
		),
	)

	result := CompareFlows(docA, docB)

	if result.RemovedBlocks != 1 {
		t.Errorf("expected 1 removed, got %d", result.RemovedBlocks)
	}
	if result.AddedBlocks != 1 {
		t.Errorf("expected 1 added, got %d", result.AddedBlocks)
	}
	if result.Similarity != 0.0 {
		t.Errorf("expected similarity 0.0, got %f", result.Similarity)
	}
}

func TestCompareFlows_SubflowOnlyInA(t *testing.T) {
	docA := fcDoc("flow-a",
		fcSub("sf-1", "Main",
			fcBlock("b-1", "Action1", models.BlockTypeAction, "HTTP", nil),
			fcBlock("b-2", "Action2", models.BlockTypeAction, "SQL", nil),
		),
	)
	docB := fcDoc("flow-b")

	result := CompareFlows(docA, docB)

	if result.RemovedBlocks != 2 {
		t.Errorf("expected 2 removed, got %d", result.RemovedBlocks)
	}
	if len(result.SubflowDiff) != 1 {
		t.Fatalf("expected 1 subflow diff, got %d", len(result.SubflowDiff))
	}
	if result.SubflowDiff[0].SubflowA != "sf-1" {
		t.Errorf("expected SubflowA=sf-1, got %s", result.SubflowDiff[0].SubflowA)
	}
}

func TestCompareFlows_ModifiedBlock(t *testing.T) {
	sfA := fcSub("sf-1", "Main",
		fcBlock("b-1", "Action1", models.BlockTypeAction, "HTTP", map[string]string{"url": "https://a.example.com"}),
	)
	sfB := fcSub("sf-1", "Main",
		fcBlock("b-1", "Action1", models.BlockTypeAction, "HTTP", map[string]string{"url": "https://b.example.com"}),
	)
	docA := fcDoc("flow-a", sfA)
	docB := fcDoc("flow-b", sfB)

	result := CompareFlows(docA, docB)

	if result.SharedBlocks != 1 {
		t.Errorf("expected 1 shared, got %d", result.SharedBlocks)
	}
	if len(result.SubflowDiff) != 1 {
		t.Fatalf("expected 1 subflow diff, got %d", len(result.SubflowDiff))
	}
	comp := result.SubflowDiff[0]
	if len(comp.BlockDiffs) != 1 {
		t.Fatalf("expected 1 block diff, got %d", len(comp.BlockDiffs))
	}
	if comp.BlockDiffs[0].Change != "modified" {
		t.Errorf("expected 'modified' change, got %s", comp.BlockDiffs[0].Change)
	}
	if comp.BlockDiffs[0].Similarity > 0 && comp.BlockDiffs[0].Similarity >= 1.0 {
		t.Errorf("expected similarity < 1.0 for modified block, got %f", comp.BlockDiffs[0].Similarity)
	}
}

func TestCompareFlows_EmptyFlows(t *testing.T) {
	docA := fcDoc("flow-a")
	docB := fcDoc("flow-b")

	result := CompareFlows(docA, docB)

	if result.SharedBlocks != 0 || result.AddedBlocks != 0 || result.RemovedBlocks != 0 {
		t.Errorf("expected all zeros for empty flows")
	}
	if len(result.SubflowDiff) != 0 {
		t.Errorf("expected 0 subflow diffs, got %d", len(result.SubflowDiff))
	}
}

func TestBlockSimilarity_DifferentRawType(t *testing.T) {
	a := &models.Block{RawType: "HTTP", Type: models.BlockTypeAction}
	b := &models.Block{RawType: "SQL", Type: models.BlockTypeAction}
	if sim := blockSimilarity(a, b); sim != 0.0 {
		t.Errorf("expected 0.0 for different raw type, got %f", sim)
	}
}

func TestBlockSimilarity_DifferentTypeSameRawType(t *testing.T) {
	a := &models.Block{RawType: "HTTP", Type: models.BlockTypeAction}
	b := &models.Block{RawType: "HTTP", Type: models.BlockTypeCondition}
	if sim := blockSimilarity(a, b); sim != 0.3 {
		t.Errorf("expected 0.3 for different type same raw type, got %f", sim)
	}
}

func TestBlockSimilarity_Identical(t *testing.T) {
	a := &models.Block{RawType: "HTTP", Type: models.BlockTypeAction, Properties: map[string]string{"url": "x"}}
	b := &models.Block{RawType: "HTTP", Type: models.BlockTypeAction, Properties: map[string]string{"url": "x"}}
	if sim := blockSimilarity(a, b); sim != 1.0 {
		t.Errorf("expected 1.0 for identical blocks, got %f", sim)
	}
}

func TestBlockSimilarity_NoProperties(t *testing.T) {
	a := &models.Block{RawType: "HTTP", Type: models.BlockTypeAction}
	b := &models.Block{RawType: "HTTP", Type: models.BlockTypeAction}
	if sim := blockSimilarity(a, b); sim != 1.0 {
		t.Errorf("expected 1.0 for no properties, got %f", sim)
	}
}

func TestBlockSimilarity_PartialMatch(t *testing.T) {
	a := &models.Block{RawType: "HTTP", Type: models.BlockTypeAction, Properties: map[string]string{"url": "x", "method": "GET"}}
	b := &models.Block{RawType: "HTTP", Type: models.BlockTypeAction, Properties: map[string]string{"url": "x", "body": "data"}}
	if sim := blockSimilarity(a, b); sim <= 0.0 || sim >= 1.0 {
		t.Errorf("expected partial similarity (0,1), got %f", sim)
	}
}
