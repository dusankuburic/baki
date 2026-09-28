package rag

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// countingEmbedder embeds like stubEmbedder ([]float32{len(text)}) and
// records every input it saw, so a test can prove no chunk was embedded
// twice and none was skipped.
type countingEmbedder struct {
	ai.Provider
	calls  int
	inputs []string
}

func (c *countingEmbedder) ID() string { return "counting-embed" }
func (c *countingEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	c.calls++
	c.inputs = append(c.inputs, text...)
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{float32(len(text[i]))}
	}
	return out, nil
}

// seedManyChunks stores n chunks of org-1 spread round-robin over docs
// documents, every chunk stranded at a width-3 zero-free vector. Ids are
// zero-padded so lexical order is numeric order.
func seedManyChunks(store *testutil.FakeBackend, docs, n int) {
	ctx := context.Background()
	for d := range docs {
		_ = store.SaveKnowledgeDocument(ctx, &interfaces.KnowledgeDocument{ID: fmt.Sprintf("doc-%02d", d), OrgID: "org-1", Filename: fmt.Sprintf("doc-%02d.md", d)})
	}
	chunks := make([]interfaces.KnowledgeChunk, n)
	for i := range n {
		chunks[i] = interfaces.KnowledgeChunk{
			ID:        fmt.Sprintf("chunk-%06d", i),
			DocID:     fmt.Sprintf("doc-%02d", i%docs),
			Content:   fmt.Sprintf("guideline number %d", i),
			Embedding: []float32{1, 1, 1},
		}
	}
	_ = store.SaveKnowledgeChunks(ctx, "", chunks)
}

// TestReindexOrg_WalksEveryPage pins the keyset walk: a corpus larger than
// one page is converted completely — every chunk re-embedded exactly once,
// every document counted, and nothing reported as truncated.
func TestReindexOrg_WalksEveryPage(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	docs, n := 3, reindexPageSize+1
	seedManyChunks(store, docs, n)
	emb := &countingEmbedder{}
	svc := guardService(store, emb)

	res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
	if err != nil {
		t.Fatalf("ReindexOrg: %v", err)
	}
	if res.Chunks != n || res.Docs != docs || res.Truncated || res.NextCursor != nil {
		t.Errorf("result = %+v, want %d chunks in %d docs, complete (no cursor)", res, n, docs)
	}
	if len(emb.inputs) != n {
		t.Errorf("embedded %d inputs for %d chunks — a chunk was skipped or embedded twice", len(emb.inputs), n)
	}
	if want := (reindexPageSize+embedBatchSize-1)/embedBatchSize + 1; emb.calls != want {
		t.Errorf("embed calls = %d, want %d (a full page of batches plus one for the remainder)", emb.calls, want)
	}
	for id, c := range store.KnowledgeChunks {
		if len(c.Embedding) != 1 || c.Embedding[0] != float32(len(c.Content)) {
			t.Fatalf("chunk %s not re-embedded: %v", id, c.Embedding)
		}
	}

	// The same corpus read through the store's own paging is complete and
	// in (DocID, ID) order, so the cursor can never skip or repeat a row.
	var walked []string
	var cursor interfaces.KnowledgeChunkCursor
	for {
		page, err := store.ListKnowledgeChunkContents(context.Background(), "org-1", cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, c := range page {
			walked = append(walked, c.DocID+"/"+c.ID)
		}
		cursor = interfaces.KnowledgeChunkCursor{DocID: page[len(page)-1].DocID, ID: page[len(page)-1].ID}
	}
	if len(walked) != n || !sort.StringsAreSorted(walked) {
		t.Errorf("paged walk returned %d rows (sorted=%v), want %d in (DocID, ID) order", len(walked), sort.StringsAreSorted(walked), n)
	}
}

// TestReindexOrg_TruncatedBeyondCeiling: at the per-request ceiling the
// result says Truncated only when chunks remain — the first ceiling-many
// (in walk order) are converted, the rest untouched, and a corpus of
// exactly the ceiling is complete.
func TestReindexOrg_TruncatedBeyondCeiling(t *testing.T) {
	prev := maxReindexChunks
	maxReindexChunks = 7
	t.Cleanup(func() { maxReindexChunks = prev })

	store := &testutil.FakeBackend{EmbeddingDim: 1}
	seedManyChunks(store, 2, maxReindexChunks+1)
	svc := guardService(store, &countingEmbedder{})

	res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
	if err != nil {
		t.Fatalf("ReindexOrg: %v", err)
	}
	if res.Chunks != maxReindexChunks || !res.Truncated || res.NextCursor == nil {
		t.Errorf("result = %+v, want %d chunks, Truncated and a NextCursor", res, maxReindexChunks)
	}
	// Walk order is (DocID, ID): doc-00's chunks first, then doc-01's; the
	// one chunk beyond the ceiling is doc-01's last.
	converted := 0
	var untouched []string
	for id, c := range store.KnowledgeChunks {
		if len(c.Embedding) == 1 {
			converted++
		} else {
			untouched = append(untouched, id)
		}
	}
	if converted != maxReindexChunks || len(untouched) != 1 || store.KnowledgeChunks[untouched[0]].DocID != "doc-01" {
		t.Errorf("converted %d chunks, untouched %v; want %d converted and doc-01's last chunk untouched", converted, untouched, maxReindexChunks)
	}

	// A re-run WITHOUT the cursor walks from the start again (same
	// ceiling, same remainder) — a client that predates the cursor sees
	// Truncated; resuming is TestReindexOrg_ResumesAcrossRequests.
	res, err = svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
	if err != nil || res.Chunks != maxReindexChunks || !res.Truncated {
		t.Errorf("second run = %+v, %v; want the same ceiling and Truncated again", res, err)
	}

	// Exactly the ceiling: complete, not truncated.
	exact := &testutil.FakeBackend{EmbeddingDim: 1}
	seedManyChunks(exact, 2, maxReindexChunks)
	if res, err := guardService(exact, &countingEmbedder{}).ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{}); err != nil || res.Chunks != maxReindexChunks || res.Truncated || res.NextCursor != nil {
		t.Errorf("corpus of exactly the ceiling = %+v, %v; want complete and not truncated", res, err)
	}
}

// gateEmbedder blocks its FIRST Embed until released, so a test can hold a
// re-index mid-flight; every later call (another org's upload, the upload
// after the release) passes straight through. Every vector is [1 0] — the
// seeded corpus's model.
type gateEmbedder struct {
	ai.Provider
	entered chan struct{}
	release chan struct{}
	first   atomic.Bool
}

func (g *gateEmbedder) ID() string { return "gate-embed" }
func (g *gateEmbedder) Embed(ctx context.Context, text []string) ([][]float32, error) {
	if g.first.CompareAndSwap(false, true) {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// TestAddDocument_RefusedWhileReindexing pins the per-org lock: an upload
// that lands while the org's re-index is embedding is refused with
// ErrKnowledgeBusy and writes nothing (it would carry the pre-switch model
// and never be seen by the re-index); once the re-index finishes, the same
// upload succeeds. Another org is not affected.
func TestAddDocument_RefusedWhileReindexing(t *testing.T) {
	store := &testutil.FakeBackend{}
	seedCorpus(store, []float32{1, 0}, "alpha rule", "beta rule")
	_ = store.SaveKnowledgeDocument(context.Background(), &interfaces.KnowledgeDocument{ID: "doc-other", OrgID: "org-2", Filename: "other.md"})
	gate := &gateEmbedder{entered: make(chan struct{}), release: make(chan struct{})}
	svc := guardService(store, gate)

	type result struct {
		res ReindexResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
		done <- result{res, err}
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("re-index never reached the embedder")
	}

	before := storedEmbeddings(store)
	if err := svc.AddDocument(context.Background(), "scope", "org-1", "new.md", "a new guideline"); !errors.Is(err, ErrKnowledgeBusy) {
		t.Fatalf("upload during re-index: err = %v, want ErrKnowledgeBusy", err)
	}
	if len(store.KnowledgeDocs) != 2 || !sameEmbeddings(before, storedEmbeddings(store)) {
		t.Error("the refused upload wrote to the store")
	}
	if _, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{}); !errors.Is(err, ErrKnowledgeBusy) {
		t.Errorf("second re-index during re-index: err = %v, want ErrKnowledgeBusy", err)
	}
	// Other orgs are independent (their own lock).
	if err := svc.AddDocument(context.Background(), "scope", "org-2", "second.md", "another org's guideline"); err != nil {
		t.Errorf("upload into another org during the re-index: %v", err)
	}

	close(gate.release)
	select {
	case r := <-done:
		if r.err != nil || r.res.Chunks != 2 {
			t.Fatalf("re-index = %+v, %v; want 2 chunks", r.res, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("re-index did not finish after release")
	}

	if err := svc.AddDocument(context.Background(), "scope", "org-1", "new.md", "a new guideline"); err != nil {
		t.Fatalf("upload after the re-index: %v", err)
	}
	if len(store.KnowledgeDocs) != 4 {
		t.Errorf("%d documents after the upload, want 4", len(store.KnowledgeDocs))
	}
}

// failingEmbedder fails every Embed with err.
type failingEmbedder struct {
	ai.Provider
	err error
}

func (f failingEmbedder) ID() string { return "failing-embed" }
func (f failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, f.err
}

// TestEmbeddingFailed_WrapsProviderCause: every indexing path (the guard
// sample, the upload, the re-index) reports a provider failure as
// ErrEmbeddingFailed wrapping the provider's cause, so the HTTP layer can
// classify it without a masked 500.
func TestEmbeddingFailed_WrapsProviderCause(t *testing.T) {
	cause := &ai.RateLimitError{RetryAfter: time.Second}
	// Non-empty corpus: the sample runs first and fails.
	store := &testutil.FakeBackend{}
	seedCorpus(store, []float32{1, 0}, "alpha rule")
	svc := guardService(store, failingEmbedder{err: cause})
	for name, err := range map[string]error{
		"upload (sample)": svc.AddDocument(context.Background(), "scope", "org-1", "new.md", "text"),
		"re-index (sample)": func() error {
			_, err := svc.ReindexOrg(context.Background(), "scope", "org-1", false, interfaces.KnowledgeChunkCursor{})
			return err
		}(),
		"re-index (forced)": func() error {
			_, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
			return err
		}(),
		"upload (empty org)": svc.AddDocument(context.Background(), "scope", "org-empty", "new.md", "text"),
	} {
		if !errors.Is(err, ErrEmbeddingFailed) || !errors.Is(err, ai.ErrRateLimited) {
			t.Errorf("%s: err = %v, want ErrEmbeddingFailed wrapping ai.ErrRateLimited", name, err)
		}
	}
	if len(store.KnowledgeDocs) != 1 {
		t.Errorf("a failed upload wrote a document: %d docs", len(store.KnowledgeDocs))
	}
}
