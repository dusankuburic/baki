package database_test

import (
	"context"
	"os"
	"testing"

	"pad-analyzer/internal/storage/database"
)

// TestSampleKnowledgeChunkEmbeddings_Postgres covers the storage half of the
// embedding-model mismatch guard: the sample is org-scoped, carries the
// stored vectors, skips chunks without an embedding array and honours n.
// Gated on DATABASE_URL (the podman harness); works with or without pgvector.
func TestSampleKnowledgeChunkEmbeddings_Postgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping knowledge sample integration test")
	}
	ctx := context.Background()
	cfg := database.DefaultConfig(dsn)
	cfg.EmbeddingDim = 3
	b, err := database.New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	db := b.DB()
	docs := map[string]string{"sample-doc": "sample-org", "sample-other-doc": "sample-other-org"}
	t.Cleanup(func() {
		for doc := range docs {
			_, _ = db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id = $1`, doc)
			_, _ = db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, doc)
		}
	})
	for doc, org := range docs {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO knowledge_documents (id, org_id, filename) VALUES ($1, $2, 'sample.txt')
			ON CONFLICT (id) DO NOTHING`, doc, org); err != nil {
			t.Fatalf("seed doc: %v", err)
		}
	}
	seed := []struct{ id, doc, emb string }{
		{"sample-1", "sample-doc", "[1,0,0]"},
		{"sample-2", "sample-doc", "[0,1,0]"},
		{"sample-empty", "sample-doc", "[]"},
		{"sample-foreign", "sample-other-doc", "[0,0,1]"},
	}
	for _, c := range seed {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO knowledge_chunks (id, doc_id, content, embedding)
			VALUES ($1, $2, $3, $4::jsonb)
			ON CONFLICT (id) DO UPDATE SET embedding = EXCLUDED.embedding`,
			c.id, c.doc, "text of "+c.id, c.emb); err != nil {
			t.Fatalf("seed chunk %s: %v", c.id, err)
		}
	}

	got, err := b.SampleKnowledgeChunkEmbeddings(ctx, "sample-org", 10)
	if err != nil {
		t.Fatalf("SampleKnowledgeChunkEmbeddings: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1 (one per document; org-scoped, empty embedding skipped): %+v", len(got), got)
	}
	for _, c := range got {
		if len(c.Embedding) != 3 || c.Content != "text of "+c.ID || c.DocID != "sample-doc" {
			t.Errorf("sampled chunk %+v lacks its content or stored embedding", c)
		}
	}
	if one, err := b.SampleKnowledgeChunkEmbeddings(ctx, "sample-org", 1); err != nil || len(one) != 1 {
		t.Errorf("n=1: %d chunks, %v", len(one), err)
	}
	if none, err := b.SampleKnowledgeChunkEmbeddings(ctx, "sample-org", 0); err != nil || len(none) != 0 {
		t.Errorf("n=0: %d chunks, %v", len(none), err)
	}
}
