package rag

import (
	"context"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// taskEmbedder records, per Embed call, whether the call was marked as a query embed.
type taskEmbedder struct {
	ai.Provider
	mu    sync.Mutex
	query []bool
}

func (e *taskEmbedder) ID() string { return "task-embed" }
func (e *taskEmbedder) Embed(ctx context.Context, text []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.query = append(e.query, ai.IsQueryEmbedding(ctx))
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{1}
	}
	return out, nil
}

// TestSearch_QueryEmbedMarkedAsQuery (R-knowledge-4): the search query is embedded under the query mark
// and everything on the document side (the upload, the sample guard) without it — Gemini pairs
// RETRIEVAL_QUERY with RETRIEVAL_DOCUMENT on that distinction.
func TestSearch_QueryEmbedMarkedAsQuery(t *testing.T) {
	store := &testutil.FakeBackend{}
	seedCorpus(store, []float32{1}, "retry policy: three attempts")
	emb := &taskEmbedder{}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))

	if _, err := svc.Search(context.Background(), "u1", "org-1", "what is the retry policy?"); err != nil {
		t.Fatal(err)
	}
	if len(emb.query) != 1 || !emb.query[0] {
		t.Fatalf("the search embedded with query marks %v, want [true]", emb.query)
	}

	if err := svc.AddDocument(context.Background(), "u1", "org-1", "notes.md", "a new guideline"); err != nil {
		t.Fatal(err)
	}
	if len(emb.query) < 2 {
		t.Fatalf("the upload embedded nothing (marks %v)", emb.query)
	}
	for i, q := range emb.query[1:] {
		if q {
			t.Errorf("document-side embed %d was marked as a query (marks %v)", i+1, emb.query)
		}
	}
}
