package rag

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// stubEmbedder embeds deterministically: one []float32{len(text)} per input.
type stubEmbedder struct {
	ai.Provider
	calls int
}

func (s *stubEmbedder) ID() string { return "stub-embed" }
func (s *stubEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	s.calls++
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{float32(len(text[i]))}
	}
	return out, nil
}

func newStubService() (*KnowledgeService, *testutil.FakeBackend, *stubEmbedder) {
	emb := &stubEmbedder{}
	store := &testutil.FakeBackend{}
	svc := NewKnowledgeService(store, nil, nil)
	svc.embedOverride = emb
	return svc, store, emb
}

// TestAddDocument_ReplacesSameFilename pins G2 replace semantics: re-uploading
// a filename supersedes the old version (one document row, chunks only from
// the newest upload) instead of leaving both versions listed and retrievable.
func TestAddDocument_ReplacesSameFilename(t *testing.T) {
	svc, store, _ := newStubService()
	ctx := context.Background()

	if err := svc.AddDocument(ctx, "scope", "org-1", "guide.md", "first version of the document"); err != nil {
		t.Fatalf("first AddDocument: %v", err)
	}
	if err := svc.AddDocument(ctx, "scope", "org-1", "guide.md", "second version with different words"); err != nil {
		t.Fatalf("second AddDocument: %v", err)
	}

	docs, err := store.ListKnowledgeDocuments(ctx, "org-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("want exactly 1 document after re-upload, got %d", len(docs))
	}
	total, _, _ := store.CountKnowledgeChunks(ctx, "org-1")
	// Both texts are short (<1000 runes) → one chunk per upload; the first
	// upload's chunk must be gone with its document.
	if total != 1 {
		t.Errorf("want 1 chunk (old version replaced), got %d", total)
	}
	contents, _ := store.ListKnowledgeChunkContents(ctx, "org-1")
	if len(contents) != 1 || !strings.Contains(contents[0].Content, "second version") {
		t.Errorf("surviving chunk is not the newest upload: %+v", contents)
	}
}

// TestAddDocument_ReplaceIsolatesOrgs: the by-name delete is org-scoped —
// another org's document with the same filename survives.
func TestAddDocument_ReplaceIsolatesOrgs(t *testing.T) {
	svc, store, _ := newStubService()
	ctx := context.Background()

	_ = svc.AddDocument(ctx, "scope", "org-A", "guide.md", "org A copy")
	_ = svc.AddDocument(ctx, "scope", "org-B", "guide.md", "org B copy")

	for _, org := range []string{"org-A", "org-B"} {
		docs, _ := store.ListKnowledgeDocuments(ctx, org)
		if len(docs) != 1 {
			t.Errorf("%s: want 1 document, got %d", org, len(docs))
		}
	}
}

// TestReindexOrg_ReembedsCorpus pins G1: the corpus is re-embedded with the
// CURRENT provider and updated in place — the recovery path for an embedding
// switch that stranded every chunk at the old dimension.
func TestReindexOrg_ReembedsCorpus(t *testing.T) {
	svc, store, emb := newStubService()
	ctx := context.Background()

	long := strings.Repeat("policy text ", 200) // forces >1 chunk at 1000 runes
	if err := svc.AddDocument(ctx, "scope", "org-1", "policy.md", long); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	total, _, _ := store.CountKnowledgeChunks(ctx, "org-1")
	if total < 2 {
		t.Fatalf("fixture should produce multiple chunks, got %d", total)
	}

	// Strand the corpus at a wrong dimension (as a provider switch would).
	for id, c := range store.KnowledgeChunks {
		c.Embedding = []float32{0, 0, 0}
		store.KnowledgeChunks[id] = c
	}

	res, err := svc.ReindexOrg(ctx, "scope", "org-1")
	if err != nil {
		t.Fatalf("ReindexOrg: %v", err)
	}
	if res.Chunks != total || res.Docs != 1 {
		t.Errorf("result = %+v, want chunks=%d docs=1", res, total)
	}
	if emb.calls == 0 {
		t.Error("re-index never called the embedder")
	}
	for id, c := range store.KnowledgeChunks {
		if len(c.Embedding) != 1 || c.Embedding[0] != float32(len(c.Content)) {
			t.Fatalf("chunk %s not re-embedded: %v", id, c.Embedding)
		}
	}
	// Idempotent on empty orgs: no corpus, no work, no error.
	empty, err := svc.ReindexOrg(ctx, "scope", "org-empty")
	if err != nil || empty.Chunks != 0 {
		t.Errorf("empty org re-index: %+v, %v", empty, err)
	}
}

// TestReindexOrg_NoProvider surfaces the machine-readable sentinel — the
// admin clicked re-index on a deployment with no embedding key.
func TestReindexOrg_NoProvider(t *testing.T) {
	svc := NewKnowledgeService(&testutil.FakeBackend{}, nil, nil)
	if _, err := svc.ReindexOrg(context.Background(), "scope", "org-1"); err == nil ||
		!strings.Contains(err.Error(), "embedding provider") {
		t.Fatalf("want embedding-provider error, got %v", err)
	}
}

// TestSearch_DimMismatchPathIsSafe: a stranded corpus (empty search result
// but chunks exist) must return empty guidelines WITHOUT error — the
// detection logs/meters; it never breaks the chat turn.
func TestSearch_DimMismatchPathIsSafe(t *testing.T) {
	svc, _, _ := newStubService()
	ctx := context.Background()
	_ = svc.AddDocument(ctx, "scope", "org-1", "g.md", "content") // 1 chunk

	// FakeBackend.SearchKnowledge returns nil → the detection path runs
	// (CountKnowledgeChunks > 0) and must not turn into an error.
	out, err := svc.Search(ctx, "scope", "org-1", "question")
	if err != nil {
		t.Fatalf("Search on stranded corpus errored: %v", err)
	}
	if out != "" {
		t.Errorf("guidelines = %q, want empty", out)
	}
}

// TestListKnowledgeDocuments_ChunkCounts: the management view carries chunk
// counts (FakeBackend parity with the Postgres LEFT JOIN).
func TestListKnowledgeDocuments_ChunkCounts(t *testing.T) {
	svc, store, _ := newStubService()
	ctx := context.Background()
	_ = svc.AddDocument(ctx, "scope", "org-1", "a.md", strings.Repeat("x ", 1500)) // 2+ chunks

	docs, err := store.ListKnowledgeDocuments(ctx, "org-1")
	if err != nil || len(docs) != 1 {
		t.Fatalf("list: %v (%d docs)", err, len(docs))
	}
	if docs[0].ChunkCount < 2 {
		t.Errorf("ChunkCount = %d, want >= 2", docs[0].ChunkCount)
	}
}
