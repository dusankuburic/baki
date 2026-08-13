package database_test

import (
	"context"
	"os"
	"testing"

	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

// TestSearchKnowledge_Pgvector_ServerSideRanking verifies the pgvector pushdown
// path: with the vector extension active, SearchKnowledge orders by cosine
// distance in the database rather than loading a 500-chunk sample into Go.
//
// Gated on DATABASE_URL (the podman harness) AND pgvector being installed; if
// either is absent the test skips rather than fails — the Go-side fallback is
// covered separately by knowledge_ranking_test.go.
func TestSearchKnowledge_Pgvector_ServerSideRanking(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping pgvector integration test")
	}
	cfg := database.DefaultConfig(dsn)
	cfg.EmbeddingDim = 3 // small fixed dim for a deterministic fixture
	b, err := database.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	if !b.HasPgvector() {
		t.Skip("pgvector not installed in this Postgres — skipping server-side ranking test")
	}

	ctx := context.Background()
	db := b.DB()
	orgID := "test-pgvector-org"
	docID := "test-pgvector-doc"
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id = $1`, docID)
		_, _ = db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID)
	})

	if _, err := db.ExecContext(ctx, `
		INSERT INTO knowledge_documents (id, org_id, filename) VALUES ($1, $2, 'test.txt')
		ON CONFLICT (id) DO NOTHING`, docID, orgID); err != nil {
		t.Fatalf("seed doc: %v", err)
	}

	// Three chunks at distinct angles to a query along the +x axis. "near" is
	// closest, "mid" is 45°, "far" is orthogonal. SaveKnowledgeChunks writes
	// embedding_vec for same-dimension chunks (all 3 here).
	chunks := []interfaces.KnowledgeChunk{
		{ID: "k-near", DocID: docID, Content: "near", Embedding: []float32{1, 0.1, 0}},
		{ID: "k-mid", DocID: docID, Content: "mid", Embedding: []float32{0.7, 0.7, 0}},
		{ID: "k-far", DocID: docID, Content: "far", Embedding: []float32{0, 1, 0}},
	}
	// SaveKnowledgeChunks runs under RLS; in this raw test harness RLS is not
	// enforced (no app.current_user_id set), so a plain insert is safe. Use the
	// public API to also exercise the embedding_vec write path.
	if err := b.SaveKnowledgeChunks(ctx, "", chunks); err != nil {
		// RLS may block the public path on some harnesses; fall back to a direct
		// insert including the vector column so the ranking is still exercised.
		for _, c := range chunks {
			vec := database.FormatVector(c.Embedding)
			if _, err := db.ExecContext(ctx,
				`INSERT INTO knowledge_chunks (id, doc_id, content, embedding, embedding_vec)
				 VALUES ($1, $2, $3, $4, $5::vector)
				 ON CONFLICT (id) DO UPDATE SET embedding_vec = EXCLUDED.embedding_vec`,
				c.ID, c.DocID, c.Content, `[]`, vec); err != nil {
				t.Fatalf("seed chunk %s: %v", c.ID, err)
			}
		}
	}

	got, err := b.SearchKnowledge(ctx, orgID, []float32{1, 0, 0}, 3)
	if err != nil {
		t.Fatalf("SearchKnowledge: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3", len(got))
	}
	if got[0].ID != "k-near" || got[2].ID != "k-far" {
		t.Errorf("server-side ranking = [%s %s %s], want near…far", got[0].ID, got[1].ID, got[2].ID)
	}
}
