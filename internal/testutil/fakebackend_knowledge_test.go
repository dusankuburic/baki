package testutil

import (
	"context"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// seedWidthCorpus stores, in one document of "org", chunk "narrow" at width 2 and chunk "wide" at width 3.
func seedWidthCorpus(t *testing.T, b *FakeBackend) {
	t.Helper()
	ctx := context.Background()
	b.SeedOrgs("org")
	if err := b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: "d1", OrgID: "org", Filename: "guide.md"}); err != nil {
		t.Fatalf("SaveKnowledgeDocument: %v", err)
	}
	if err := b.SaveKnowledgeChunks(ctx, "", []interfaces.KnowledgeChunk{
		{ID: "narrow", DocID: "d1", Content: "fits", Embedding: []float32{1, 0}, EmbeddingIdentity: "A"},
		{ID: "wide", DocID: "d1", Content: "too wide", Embedding: []float32{1, 0, 0}, EmbeddingIdentity: "A"},
	}); err != nil {
		t.Fatalf("SaveKnowledgeChunks: %v", err)
	}
}

func chunkIDs(chunks []interfaces.KnowledgeChunk) []string {
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = c.ID
	}
	return ids
}

// TestFakeBackend_VectorIndexMirrorsPostgres (R-config-8): with a vector index a chunk is in it at the configured
// width alone, the unindexed page and the width probe see the rows outside it; without pgvector nothing is in an
// index, the probe answers 0 and the unindexed page is every chunk — as the Postgres statements behave.
func TestFakeBackend_VectorIndexMirrorsPostgres(t *testing.T) {
	ctx := context.Background()
	b := &FakeBackend{EmbeddingDim: 2}
	seedWidthCorpus(t, b)
	if !b.HasPgvector() {
		t.Error("HasPgvector = false by default; the fake models a store with pgvector unless NoPgvector is set")
	}
	if total, indexed, err := b.CountKnowledgeChunks(ctx, "org"); err != nil || total != 2 || indexed != 1 {
		t.Errorf("CountKnowledgeChunks = %d, %d, %v; want 2 total, 1 indexed", total, indexed, err)
	}
	page, err := b.ListUnindexedKnowledgeChunkContents(ctx, "org", interfaces.KnowledgeChunkCursor{}, 0)
	if err != nil || len(page) != 1 || page[0].ID != "wide" {
		t.Errorf("ListUnindexedKnowledgeChunkContents = %v, %v; want the wide chunk alone", chunkIDs(page), err)
	}
	if width, err := b.UnindexedKnowledgeChunkWidth(ctx, "org"); err != nil || width != 3 {
		t.Errorf("UnindexedKnowledgeChunkWidth = %d, %v; want 3", width, err)
	}
	if width, err := b.UnindexedKnowledgeChunkWidth(ctx, "other-org"); err != nil || width != 0 {
		t.Errorf("UnindexedKnowledgeChunkWidth(other org) = %d, %v; want 0", width, err)
	}

	none := &FakeBackend{EmbeddingDim: 2, NoPgvector: true}
	seedWidthCorpus(t, none)
	if none.HasPgvector() {
		t.Error("HasPgvector = true with NoPgvector set")
	}
	if total, indexed, err := none.CountKnowledgeChunks(ctx, "org"); err != nil || total != 2 || indexed != 0 {
		t.Errorf("CountKnowledgeChunks without pgvector = %d, %d, %v; want 2 total, 0 indexed", total, indexed, err)
	}
	docs, _ := none.ListKnowledgeDocuments(ctx, "org", 0, 0)
	if len(docs) != 1 || docs[0].ChunkCount != 2 || docs[0].VectorIndexed != 0 {
		t.Errorf("ListKnowledgeDocuments without pgvector = %+v; want 2 chunks, 0 indexed", docs)
	}
	page, err = none.ListUnindexedKnowledgeChunkContents(ctx, "org", interfaces.KnowledgeChunkCursor{}, 0)
	if err != nil || len(page) != 2 {
		t.Errorf("ListUnindexedKnowledgeChunkContents without pgvector = %v, %v; want every chunk", chunkIDs(page), err)
	}
	if width, err := none.UnindexedKnowledgeChunkWidth(ctx, "org"); err != nil || width != 0 {
		t.Errorf("UnindexedKnowledgeChunkWidth without pgvector = %d, %v; want 0", width, err)
	}
}
