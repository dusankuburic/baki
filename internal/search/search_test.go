package search

import (
	"fmt"
	"testing"

	"pad-analyzer/internal/models"
)

// BenchmarkSearchFuzzy exercises the fuzzy fallback over a large token index.
// The length pre-filter skips the Levenshtein DP for tokens whose length is too
// far from the query token, which dominates cost on a big index.
func BenchmarkSearchFuzzy(b *testing.B) {
	blocks := make([]models.Block, 0, 2000)
	for i := 0; i < 2000; i++ {
		blocks = append(blocks, models.Block{
			ID:      fmt.Sprintf("b%d", i),
			Name:    fmt.Sprintf("Action variant %d alpha beta gamma delta", i),
			Type:    models.BlockTypeAction,
			RawType: fmt.Sprintf("WebAutomation.Operation%d", i),
		})
	}
	doc := &models.FlowDocument{ID: "bench", Subflows: []models.Subflow{{ID: "sf", Name: "Main", Blocks: blocks}}}
	idx := NewSearchIndex(doc.ID, doc)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.Search(models.SearchQuery{Text: "actionx", Fuzzy: true, MaxResults: 10})
	}
}

func makeTestDoc() *models.FlowDocument {
	return &models.FlowDocument{
		ID:   "test-flow",
		Name: "Test Flow",
		Subflows: []models.Subflow{
			{
				ID:   "sf-main",
				Name: "Main",
				Blocks: []models.Block{
					{
						ID: "b1", Name: "Click on element", Type: models.BlockTypeAction,
						RawType: "WebAutomation.Click.Click", SubflowID: "sf-main",
						Properties: map[string]string{"Selector": "Submit button"},
						Variables:  []string{"WebPage"},
					},
					{
						ID: "b2", Name: "Read from Excel", Type: models.BlockTypeAction,
						RawType: "Excel.ReadFromExcelCells", SubflowID: "sf-main",
						Properties: map[string]string{"Cell": "A1"},
					},
					{
						ID: "b3", Name: "Loop through items", Type: models.BlockTypeLoop,
						RawType: "Loop.ForEach", SubflowID: "sf-main",
						Children: []models.Block{
							{
								ID: "b3a", Name: "If condition", Type: models.BlockTypeCondition,
								RawType: "Condition.If", SubflowID: "sf-main",
							},
						},
					},
					{
						ID: "b4", Name: "Set variable", Type: models.BlockTypeVariable,
						RawType: "Variables.SetVariable", SubflowID: "sf-main",
						Variables: []string{"Counter"},
					},
					{
						ID: "b5", Name: "On block error", Type: models.BlockTypeErrorHandler,
						RawType: "ErrorHandler.OnBlockError", SubflowID: "sf-main",
					},
				},
			},
		},
	}
}

func TestIndexBuild(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	if idx.FlowID() != "test-flow" {
		t.Errorf("FlowID = %q, want %q", idx.FlowID(), "test-flow")
	}

	if len(idx.blocks) != 6 {
		t.Errorf("indexed %d blocks, want 6", len(idx.blocks))
	}

	if len(idx.tokens) == 0 {
		t.Error("no tokens in index")
	}

	if idx.blockSubflow["b1"] != "sf-main" {
		t.Errorf("blockSubflow[b1] = %q, want %q", idx.blockSubflow["b1"], "sf-main")
	}
}

func TestSearchExactNameMatch(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "Click on element", MaxResults: 10})
	if results.TotalCount == 0 {
		t.Fatal("expected results for exact name match")
	}

	if results.Results[0].BlockID != "b1" {
		t.Errorf("first result = %q, want %q", results.Results[0].BlockID, "b1")
	}

	if results.Results[0].Score != 120 {
		t.Errorf("exact match score = %d, want 120 (100 exact + 20 all tokens)", results.Results[0].Score)
	}
}

func TestSearchPartialMatch(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "click", MaxResults: 10})
	if results.TotalCount == 0 {
		t.Fatal("expected results for partial match 'click'")
	}

	found := false
	for _, r := range results.Results {
		if r.BlockID == "b1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("b1 (Click on element) not found for query 'click'")
	}
}

func TestSearchPropertyMatch(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "submit", MaxResults: 10})
	if results.TotalCount == 0 {
		t.Fatal("expected results for property value 'submit'")
	}

	found := false
	for _, r := range results.Results {
		if r.BlockID == "b1" {
			found = true
			if r.MatchedField != "property:Selector" {
				t.Errorf("matchedField = %q, want %q", r.MatchedField, "property:Selector")
			}
			break
		}
	}
	if !found {
		t.Error("b1 not found for property value 'submit'")
	}
}

func TestSearchVariableMatch(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "counter", MaxResults: 10})
	if results.TotalCount == 0 {
		t.Fatal("expected results for variable 'counter'")
	}

	found := false
	for _, r := range results.Results {
		if r.BlockID == "b4" {
			found = true
			break
		}
	}
	if !found {
		t.Error("b4 (Set variable Counter) not found for query 'counter'")
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "", MaxResults: 10})
	if results.TotalCount != 0 {
		t.Errorf("empty query should return 0 results, got %d", results.TotalCount)
	}
}

func TestSearchNoMatch(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "zzzznonexistent", MaxResults: 10})
	if results.TotalCount != 0 {
		t.Errorf("nonexistent query should return 0 results, got %d", results.TotalCount)
	}
}

func TestSearchFuzzyMatch(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "clikc", Fuzzy: true, MaxResults: 10})
	if results.TotalCount == 0 {
		t.Fatal("expected fuzzy results for 'clikc'")
	}

	found := false
	for _, r := range results.Results {
		if r.BlockID == "b1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("b1 not found for fuzzy query 'clikc'")
	}
}

func TestSearchBlockTypeFilter(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{
		Text:       "loop",
		BlockTypes: []models.BlockType{models.BlockTypeLoop},
		MaxResults: 10,
	})

	for _, r := range results.Results {
		block := idx.blocks[r.BlockID]
		if block.Type != models.BlockTypeLoop {
			t.Errorf("got block type %q, want LOOP", block.Type)
		}
	}
}

func TestSearchHighlights(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "click", MaxResults: 10})
	if results.TotalCount == 0 {
		t.Fatal("expected results for 'click'")
	}

	for _, r := range results.Results {
		if r.BlockID == "b1" {
			if len(r.Highlights) == 0 {
				t.Error("expected highlights for b1")
			}
			h := r.Highlights[0]
			text := "Click on element"
			if text[h.Start:h.End] != "Click" {
				t.Errorf("highlight text = %q, want %q", text[h.Start:h.End], "Click")
			}
			break
		}
	}
}

func TestSearchMaxResults(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "a", MaxResults: 2})
	if len(results.Results) > 2 {
		t.Errorf("got %d results, want at most 2", len(results.Results))
	}
}

func TestSearchDuration(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "click", MaxResults: 10})
	if results.DurationMs < 0 {
		t.Errorf("DurationMs = %d, want >= 0", results.DurationMs)
	}
}

func TestSearchNestedBlock(t *testing.T) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)

	results := idx.Search(models.SearchQuery{Text: "condition", MaxResults: 10})
	found := false
	for _, r := range results.Results {
		if r.BlockID == "b3a" {
			found = true
			break
		}
	}
	if !found {
		t.Error("nested block b3a (If condition) not found")
	}
}

func BenchmarkIndexBuild(b *testing.B) {
	doc := makeTestDoc()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		NewSearchIndex(doc.ID, doc)
	}
}

func BenchmarkSearch(b *testing.B) {
	doc := makeTestDoc()
	idx := NewSearchIndex(doc.ID, doc)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx.Search(models.SearchQuery{Text: "click element", MaxResults: 100})
	}
}
