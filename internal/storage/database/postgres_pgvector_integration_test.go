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
	// "far" is exactly orthogonal to the query — cosine similarity 0.0, below
	// the relevance floor both paths apply — so two chunks come back, ordered
	// nearest first. The original expectation here was 3 with far last, which
	// contradicted the documented cutoff on BOTH implementations; it went
	// unnoticed because this test skips wherever pgvector is absent, including
	// CI, so it had never executed.
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2 (k-far is below the relevance floor)", len(got))
	}
	if got[0].ID != "k-near" || got[1].ID != "k-mid" {
		t.Errorf("server-side ranking = [%s %s], want [k-near k-mid]", got[0].ID, got[1].ID)
	}
	for _, c := range got {
		if c.ID == "k-far" {
			t.Error("orthogonal chunk k-far should have been filtered out as irrelevant")
		}
	}
}
