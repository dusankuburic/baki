package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// lowerReindexLimits shrinks the page size and the per-request ceiling for
// one test, so "a corpus above 10 000 chunks" is a few dozen rows.
func lowerReindexLimits(t *testing.T, page, ceiling int) {
	t.Helper()
	prevPage, prevCeiling := reindexPageSize, maxReindexChunks
	reindexPageSize, maxReindexChunks = page, ceiling
	t.Cleanup(func() { reindexPageSize, maxReindexChunks = prevPage, prevCeiling })
}

// walkOrder is the org's chunk ids in the re-index walk order (DocID, ID).
func walkOrder(store *testutil.FakeBackend) []string {
	var ids []string
	for id := range store.KnowledgeChunks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := store.KnowledgeChunks[ids[i]], store.KnowledgeChunks[ids[j]]
		if a.DocID != b.DocID {
			return a.DocID < b.DocID
		}
		return a.ID < b.ID
	})
	return ids
}

// runReindexToEnd drives a resumable re-index the way the settings panel
// does: post, then post again with nextCursor until none comes back.
func runReindexToEnd(t *testing.T, svc *KnowledgeService, force bool) []ReindexResult {
	t.Helper()
	var (
		out    []ReindexResult
		cursor interfaces.KnowledgeChunkCursor
	)
	for range 100 {
		res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", force, cursor)
		if err != nil {
			t.Fatalf("request %d (cursor %+v): %v", len(out)+1, cursor, err)
		}
		out = append(out, res)
		if res.NextCursor == nil {
			return out
		}
		if !res.Truncated {
			t.Errorf("request %d: NextCursor without Truncated — older clients would believe it complete", len(out))
		}
		cursor = *res.NextCursor
	}
	t.Fatal("re-index never finished")
	return nil
}

// TestReindexOrg_ResumesAcrossRequests pins R-7: a corpus above the
// per-request ceiling completes across requests through the cursor — every
// chunk re-embedded exactly once by the walk, nothing skipped, the last
// answer carrying no cursor — and the cursor's JSON is {docId, id}.
func TestReindexOrg_ResumesAcrossRequests(t *testing.T) {
	lowerReindexLimits(t, 3, 7)
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	const n = 20
	seedManyChunks(store, 2, n)
	emb := &countingEmbedder{}
	svc := guardService(store, emb)

	results := runReindexToEnd(t, svc, false)
	if len(results) != 3 {
		t.Fatalf("%d requests, want 3 (7 + 7 + 6): %+v", len(results), results)
	}
	total := 0
	for _, r := range results {
		total += r.Chunks
	}
	if total != n || results[2].Chunks != 6 || results[2].Truncated {
		t.Errorf("chunks per request = %+v, want 20 in total and a complete last answer", results)
	}
	for id, c := range store.KnowledgeChunks {
		if len(c.Embedding) != 1 || c.Embedding[0] != float32(len(c.Content)) {
			t.Errorf("chunk %s not re-embedded: %v", id, c.Embedding)
		}
	}
	// The walk embedded each chunk once. On top: the first request's guard
	// sample (one chunk per document, 2) and, per resumed request, the
	// corpusSampleSize converted chunks its guard compares.
	if want := n + 2 + corpusSampleSize*(len(results)-1); len(emb.inputs) != want {
		t.Errorf("embedded %d inputs, want %d (%d chunks once + guard samples)", len(emb.inputs), want, n)
	}

	// The first answer's cursor is the 7th chunk in walk order.
	order := walkOrder(store)
	first := results[0].NextCursor
	if first == nil || first.ID != order[6] || first.DocID != store.KnowledgeChunks[order[6]].DocID {
		t.Errorf("first NextCursor = %+v, want the 7th chunk %s", first, order[6])
	}
	b, err := json.Marshal(results[0])
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`"nextCursor":{"docId":%q,"id":%q}`, first.DocID, first.ID)
	if !strings.Contains(string(b), want) || !strings.Contains(string(b), `"truncated":true`) {
		t.Errorf("JSON = %s, want %s and truncated", b, want)
	}
	if b, _ := json.Marshal(results[2]); strings.Contains(string(b), "nextCursor") || strings.Contains(string(b), "truncated") {
		t.Errorf("complete answer JSON = %s, want no nextCursor / truncated", b)
	}
}

// TestReindexOrg_PageFailureKeepsEarlierPages pins the per-page commit: a
// failure on page 3 — the store's commit or the provider — leaves pages 1-2
// converted (each page is its own write) and the rest untouched.
func TestReindexOrg_PageFailureKeepsEarlierPages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(store *testutil.FakeBackend) ai.Provider
	}{
		{"commit fails", func(store *testutil.FakeBackend) ai.Provider {
			store.UpdateKnowledgeEmbeddingsFailAt = 3
			return &countingEmbedder{}
		}},
		{"embedding fails", func(*testutil.FakeBackend) ai.Provider {
			return &failOnCallEmbedder{failAt: 3, err: ai.ErrProviderDown}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lowerReindexLimits(t, 3, 10000)
			store := &testutil.FakeBackend{EmbeddingDim: 1}
			seedManyChunks(store, 2, 10)
			svc := guardService(store, tc.setup(store))

			if _, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{}); err == nil {
				t.Fatal("re-index succeeded, want the page-3 failure")
			}
			for i, id := range walkOrder(store) {
				converted := len(store.KnowledgeChunks[id].Embedding) == 1
				if want := i < 6; converted != want {
					t.Errorf("chunk #%d (%s) converted = %v, want %v (pages 1-2 kept, the rest untouched)", i+1, id, converted, want)
				}
			}
		})
	}
}

// failOnCallEmbedder embeds like countingEmbedder but fails its failAt-th
// call (1-based) with err.
type failOnCallEmbedder struct {
	ai.Provider
	calls, failAt int
	err           error
}

func (f *failOnCallEmbedder) ID() string { return "fail-on-call" }
func (f *failOnCallEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	f.calls++
	if f.calls == f.failAt {
		return nil, f.err
	}
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{float32(len(text[i]))}
	}
	return out, nil
}

// cancelOnCallEmbedder cancels the request context on its cancelAt-th call
// and fails with the context's error — the request deadline striking
// mid-walk.
type cancelOnCallEmbedder struct {
	failOnCallEmbedder
	cancel context.CancelFunc
}

func (c *cancelOnCallEmbedder) Embed(ctx context.Context, text []string) ([][]float32, error) {
	if c.calls+1 == c.failAt {
		c.calls++
		c.cancel()
		return nil, ctx.Err()
	}
	return c.failOnCallEmbedder.Embed(ctx, text)
}

// TestReindexOrg_DeadlineAfterAPageIsResumable: the request's deadline
// cutting the walk after committed pages answers with those pages and a
// NextCursor (the client resumes instead of paying for them again); cut
// before any page, it is an error the handler maps to 504.
func TestReindexOrg_DeadlineAfterAPageIsResumable(t *testing.T) {
	lowerReindexLimits(t, 3, 10000)
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	seedManyChunks(store, 2, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emb := &cancelOnCallEmbedder{failOnCallEmbedder: failOnCallEmbedder{failAt: 3}, cancel: cancel}
	svc := guardService(store, emb)

	res, err := svc.ReindexOrg(ctx, "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
	if err != nil {
		t.Fatalf("ReindexOrg: %v, want the committed progress", err)
	}
	order := walkOrder(store)
	if res.Chunks != 6 || res.NextCursor == nil || res.NextCursor.ID != order[5] || !res.Truncated {
		t.Fatalf("result = %+v, want 6 chunks and a cursor at %s", res, order[5])
	}
	// Resuming from it converts the rest.
	rest, err := guardService(store, &countingEmbedder{}).ReindexOrg(context.Background(), "scope", "org-1", true, *res.NextCursor)
	if err != nil || rest.Chunks != 4 || rest.NextCursor != nil {
		t.Fatalf("resume = %+v, %v; want the remaining 4 chunks", rest, err)
	}

	// Before any page committed: an error carrying the context's cause.
	fresh := &testutil.FakeBackend{EmbeddingDim: 1}
	seedManyChunks(fresh, 2, 10)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	early := &cancelOnCallEmbedder{failOnCallEmbedder: failOnCallEmbedder{failAt: 1}, cancel: cancel2}
	_, err = guardService(fresh, early).ReindexOrg(ctx2, "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

// seedModelCorpus stores docs×per chunks of org-1 across documents
// doc-00.., every stored vector vec (one model's output).
func seedModelCorpus(store *testutil.FakeBackend, docs, per int, vec []float32) {
	ctx := context.Background()
	var chunks []interfaces.KnowledgeChunk
	for d := range docs {
		doc := fmt.Sprintf("doc-%02d", d)
		_ = store.SaveKnowledgeDocument(ctx, &interfaces.KnowledgeDocument{ID: doc, OrgID: "org-1", Filename: doc + ".md"})
		for i := range per {
			chunks = append(chunks, interfaces.KnowledgeChunk{
				ID: fmt.Sprintf("%s-c%d", doc, i), DocID: doc, Content: fmt.Sprintf("rule %d of %s", i, doc),
				Embedding: append([]float32(nil), vec...),
			})
		}
	}
	_ = store.SaveKnowledgeChunks(ctx, "", chunks)
}

// TestReindexOrg_GuardOnlyOnFirstRequest pins the resumed-request guard: a
// forced switch A → B that stops at the ceiling leaves a mixed corpus the
// first-request guard would refuse, yet B's continuation passes unforced
// (the first request's decision carries over); a caller on model A cannot
// continue B's run even with force (the converted part would be stranded
// on B), and neither changes anything when refused.
func TestReindexOrg_GuardOnlyOnFirstRequest(t *testing.T) {
	lowerReindexLimits(t, 2, 5)
	modelA, modelB := []float32{1, 0}, []float32{0, 1}
	store := &testutil.FakeBackend{}
	seedModelCorpus(store, 3, 4, modelA)
	svcB := guardService(store, &fixedEmbedder{vec: modelB})
	svcA := guardService(store, &fixedEmbedder{vec: modelA})
	zero := interfaces.KnowledgeChunkCursor{}

	if _, err := svcB.ReindexOrg(context.Background(), "scope", "org-1", false, zero); !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Fatalf("unforced first request: err = %v, want ErrEmbeddingModelChanged", err)
	}
	first, err := svcB.ReindexOrg(context.Background(), "scope", "org-1", true, zero)
	if err != nil || first.NextCursor == nil {
		t.Fatalf("forced first request = %+v, %v; want a partial run with a cursor", first, err)
	}
	// The corpus is now mixed: judged as a FIRST request it is refused.
	if _, err := svcB.ReindexOrg(context.Background(), "scope", "org-1", false, zero); !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Fatalf("unforced restart on the mixed corpus: err = %v, want ErrEmbeddingModelChanged", err)
	}

	// Model A may not continue B's run, forced or not.
	before := storedEmbeddings(store)
	for _, force := range []bool{false, true} {
		if _, err := svcA.ReindexOrg(context.Background(), "scope", "org-1", force, *first.NextCursor); !errors.Is(err, ErrEmbeddingModelChanged) {
			t.Errorf("model A continuing (force=%v): err = %v, want ErrEmbeddingModelChanged", force, err)
		}
	}
	if !sameEmbeddings(before, storedEmbeddings(store)) {
		t.Fatal("a refused continuation changed stored embeddings")
	}

	// B continues unforced to the end.
	cursor := *first.NextCursor
	for range 10 {
		res, err := svcB.ReindexOrg(context.Background(), "scope", "org-1", false, cursor)
		if err != nil {
			t.Fatalf("B continuing from %+v: %v", cursor, err)
		}
		if res.NextCursor == nil {
			break
		}
		cursor = *res.NextCursor
	}
	for id, v := range storedEmbeddings(store) {
		if v[0] != 0 || v[1] != 1 {
			t.Errorf("chunk %s = %v, want model B everywhere", id, v)
		}
	}
}

// TestReindexOrg_ResumeWithNothingConvertedIsAFirstRequest: a cursor with
// no converted chunk at or before it (the documents were deleted between
// requests; here: a cursor before every row) has nothing to compare, so the
// continuation is judged like a first request — refused unforced over
// another model's corpus, allowed forced.
func TestReindexOrg_ResumeWithNothingConvertedIsAFirstRequest(t *testing.T) {
	store := &testutil.FakeBackend{}
	seedModelCorpus(store, 2, 2, []float32{1, 0})
	svc := guardService(store, &fixedEmbedder{vec: []float32{0, 1}})
	before := interfaces.KnowledgeChunkCursor{DocID: "a", ID: "a"}
	if _, err := svc.ReindexOrg(context.Background(), "scope", "org-1", false, before); !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Fatalf("unforced: err = %v, want ErrEmbeddingModelChanged", err)
	}
	res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, before)
	if err != nil || res.Chunks != 4 {
		t.Fatalf("forced = %+v, %v; want all 4 chunks", res, err)
	}
}
