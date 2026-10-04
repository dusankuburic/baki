package search

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"pad-core/models"
)

// refIndex is the pre-refactor index shape — the (unchanged) token map plus
// the block map and block→subflow map the index used to carry — and
// refSearch is the pre-refactor Search over it, kept verbatim as the oracle:
// the refactor moved block resolution onto the document, and /search must
// return the same results, fields and order as before.
type refIndex struct {
	tokens       map[string][]string
	blocks       map[string]*models.Block
	blockSubflow map[string]string
}

func newRefIndex(doc *models.FlowDocument) *refIndex {
	ri := &refIndex{
		tokens:       NewSearchIndex(doc.ID, doc).tokens,
		blocks:       make(map[string]*models.Block),
		blockSubflow: make(map[string]string),
	}
	var walk func(b *models.Block, subflowID string)
	walk = func(b *models.Block, subflowID string) {
		ri.blocks[b.ID] = b
		ri.blockSubflow[b.ID] = subflowID
		for i := range b.Children {
			walk(&b.Children[i], subflowID)
		}
	}
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		for j := range sf.Blocks {
			walk(&sf.Blocks[j], sf.ID)
		}
	}
	return ri
}

func (ri *refIndex) search(query models.SearchQuery) *models.SearchResults {
	text := strings.ToLower(strings.TrimSpace(query.Text))
	if text == "" {
		return &models.SearchResults{Query: query}
	}
	queryTokens := tokenize(text)
	if len(queryTokens) == 0 {
		return &models.SearchResults{Query: query}
	}
	candidates := make(map[string]int)
	fuzzyHits := make(map[string]int)
	for _, qt := range queryTokens {
		if ids, ok := ri.tokens[qt]; ok {
			for _, blockID := range ids {
				candidates[blockID]++
			}
		} else if query.Fuzzy && len(qt) >= 4 {
			for token, blockIDs := range ri.tokens {
				if d := len(qt) - len(token); d > fuzzyMaxDistance || d < -fuzzyMaxDistance {
					continue
				}
				if Levenshtein(qt, token) <= fuzzyMaxDistance {
					for _, id := range blockIDs {
						candidates[id]++
						fuzzyHits[id]++
					}
				}
			}
		}
	}
	typeFilter := make(map[models.BlockType]bool)
	for _, bt := range query.BlockTypes {
		typeFilter[bt] = true
	}
	var results []models.SearchResult
	for blockID, hits := range candidates {
		block := ri.blocks[blockID]
		if block == nil {
			continue
		}
		if len(typeFilter) > 0 && !typeFilter[block.Type] {
			continue
		}
		isFuzzy := fuzzyHits[blockID] > 0
		score := computeScore(block, text, hits, len(queryTokens), isFuzzy)
		if score <= 0 {
			continue
		}
		results = append(results, models.SearchResult{
			BlockID:      blockID,
			SubflowID:    ri.blockSubflow[blockID],
			MatchedField: detectMatchedField(block, text, queryTokens),
			MatchedText:  block.Name,
			Score:        score,
			Highlights:   computeHighlights(block.Name, queryTokens),
		})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if query.MaxResults > 0 && len(results) > query.MaxResults {
		results = results[:query.MaxResults]
	}
	return &models.SearchResults{Query: query, Results: results, TotalCount: len(results)}
}

// canonical makes two result sets comparable: the duration is dropped; ties
// (equal scores — unordered before and after the refactor, as the sort is not
// stable and candidates come from a map) are ordered by block id; and a
// property match is reduced to "property", because detectMatchedField picks
// the key by ranging over the block's Properties map, so WHICH of several
// matching properties it names was never deterministic either (the
// web_automation sample's "Show Message Box" names Icon, Buttons or
// DefaultButton from run to run).
func canonical(res *models.SearchResults) *models.SearchResults {
	out := *res
	out.DurationMs = 0
	out.Results = append([]models.SearchResult(nil), res.Results...)
	for i := range out.Results {
		if strings.HasPrefix(out.Results[i].MatchedField, "property:") {
			out.Results[i].MatchedField = "property"
		}
	}
	sort.Slice(out.Results, func(i, j int) bool {
		if out.Results[i].Score != out.Results[j].Score {
			return out.Results[i].Score > out.Results[j].Score
		}
		return out.Results[i].BlockID < out.Results[j].BlockID
	})
	return &out
}

// unindexed is doc without its lookup maps: the shape of a hand-built or
// not-yet-RebuildIndexes'd document, which Search resolves by walking.
func unindexed(doc *models.FlowDocument) *models.FlowDocument {
	cp := *doc
	cp.BlocksByID, cp.BlockSubflow, cp.SubflowsByID = nil, nil, nil
	return &cp
}

func nestedDuplicateIDDoc() *models.FlowDocument {
	return &models.FlowDocument{ID: "nested", Subflows: []models.Subflow{
		{ID: "sf-a", Blocks: []models.Block{{
			ID: "p", Name: "Loop over HTTPServer rows", Type: models.BlockTypeLoop, RawType: "LOOP",
			Variables:  []string{"RowIndex", "row_count"},
			Properties: map[string]string{"From": "1", "To": "%RowCount%", "Note": "Ünïcödé ÀBC 日本"},
			Children: []models.Block{
				{ID: "c1", Name: "Click row", Type: models.BlockTypeAction, RawType: "WebAutomation.Click"},
				{ID: "p", Name: "duplicate id reuses parent", Type: models.BlockTypeAction, RawType: "Variables.SetVariable"},
			},
		}}},
		{ID: "sf-b", Blocks: []models.Block{
			{ID: "c1", Name: "Click row in another subflow", Type: models.BlockTypeAction, RawType: "WebAutomation.Click"},
			{ID: "q", Name: "Set variable rows", Type: models.BlockTypeVariable, RawType: "Variables.SetVariable", Variables: []string{"rows"}},
		}},
	}}
}

// TestSearch_MatchesPreRefactorResults runs every query shape (plain, fuzzy,
// type-filtered, capped) over the sample flows — with and without their
// lookup maps — and the hand-built fixtures, and requires the refactored
// Search to return exactly what the pre-refactor one did.
func TestSearch_MatchesPreRefactorResults(t *testing.T) {
	var docs []*models.FlowDocument
	for _, doc := range parseSamples(t) {
		docs = append(docs, doc)
		// The walk path (no lookup maps) is covered on the small samples;
		// fuzzy queries over the 10k-block sample twice would dominate the
		// test's runtime for no extra coverage.
		if doc.Metadata.BlockCount < 2000 {
			docs = append(docs, unindexed(doc))
		}
	}
	docs = append(docs, makeTestDoc(), nestedDuplicateIDDoc())
	texts := []string{
		"click", "Click on element", "excel", "set variable", "loop", "message", "var", "http",
		"keyboard press", "Display.ShowMessageBox", "folder", "a", "zz", "日本", "row", "submit",
		"clikc", "keybord", "variabel", "messge", "", "   ", "!!!", //nolint:misspell // deliberate typo queries (fuzzy match)
	}
	filters := [][]models.BlockType{
		nil,
		{models.BlockTypeAction},
		{models.BlockTypeLoop, models.BlockTypeCondition, models.BlockTypeVariable},
	}
	queries := 0
	for _, doc := range docs {
		idx := NewSearchIndex(doc.ID, doc)
		ref := newRefIndex(doc)
		for _, text := range texts {
			for _, fuzzy := range []bool{false, true} {
				for _, filter := range filters {
					q := models.SearchQuery{Text: text, Fuzzy: fuzzy, BlockTypes: filter}
					queries++
					got, want := canonical(idx.Search(doc, q)), canonical(ref.search(q))
					if !reflect.DeepEqual(got, want) {
						t.Errorf("%s (indexed=%v) query %+v: results differ\n got: %+v\nwant: %+v", doc.Name, doc.BlocksByID != nil, q, got, want)
					}
					// A cap keeps the top-N by score; ties within the cut are
					// unordered either way, so compare the score sequence.
					q.MaxResults = 5
					capped, refCapped := idx.Search(doc, q), ref.search(q)
					if len(capped.Results) != len(refCapped.Results) || capped.TotalCount != refCapped.TotalCount {
						t.Errorf("%s query %+v: %d results (total %d), reference %d (total %d)", doc.Name, q, len(capped.Results), capped.TotalCount, len(refCapped.Results), refCapped.TotalCount)
						continue
					}
					for i := range capped.Results {
						if capped.Results[i].Score != refCapped.Results[i].Score {
							t.Errorf("%s query %+v: score sequence differs at %d: %d vs %d", doc.Name, q, i, capped.Results[i].Score, refCapped.Results[i].Score)
							break
						}
					}
				}
			}
		}
	}
	t.Logf("%d queries over %d documents matched the pre-refactor results", queries, len(docs))
}

// TestSearch_ResolvesOnTheDocument pins the two lookup paths directly: the
// parsed document's maps and the walk over an un-indexed one give the same
// block and subflow, a duplicated id resolves to the last block carrying it
// (as the old block map did), and an unknown id is skipped, not resolved.
func TestSearch_ResolvesOnTheDocument(t *testing.T) {
	doc := nestedDuplicateIDDoc()
	doc.RebuildIndexes()
	indexed, walked := blockLookup{doc: doc}, blockLookup{doc: unindexed(doc)}
	for _, id := range []string{"p", "c1", "q"} {
		b1, sf1 := indexed.find(id)
		b2, sf2 := walked.find(id)
		if b1 == nil || b2 == nil || b1 != b2 || sf1 != sf2 {
			t.Errorf("id %q: indexed (%v, %q) vs walked (%v, %q)", id, b1, sf1, b2, sf2)
		}
	}
	if b, sf := walked.find("p"); b == nil || b.Name != "duplicate id reuses parent" || sf != "sf-a" {
		t.Errorf("duplicate id resolved to %+v in %q, want the last block carrying it in sf-a", b, sf)
	}
	if b, sf := walked.find("c1"); b == nil || b.Name != "Click row in another subflow" || sf != "sf-b" {
		t.Errorf("duplicate id across subflows resolved to %+v in %q, want the last one in sf-b", b, sf)
	}
	if b, sf := indexed.find("missing"); b != nil || sf != "" {
		t.Errorf("unknown id resolved to %+v %q", b, sf)
	}
	var none blockLookup
	if b, _ := none.find("p"); b != nil {
		t.Error("a nil document resolved a block")
	}
}
