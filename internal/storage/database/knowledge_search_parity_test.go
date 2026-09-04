package database

import (
	"context"
	"os"
	"testing"
)

// TestSearchKnowledge_PathParity pins the two knowledge-search implementations
// to the same answer.
//
// pgvector is a PERFORMANCE optimization: it pushes the similarity ordering and
// LIMIT into the database instead of loading candidate chunks into Go. An
// optimization that changes results is a bug, and this one did — the SQL
// predicate filtered on cosine DISTANCE < 1.0 (similarity > 0) while the Go
// ranker kept similarity >= 0.5 (distance <= 0.5). Every chunk scoring between
// those two bounds was returned by one path and dropped by the other, so the
// same question answered differently depending on whether the extension
// happened to be installed on that deployment.
//
// The fixture below straddles exactly that window: "mid-low" sits at
// similarity ~0.30, inside the old gap. Both paths must now agree to exclude
// it.
func TestSearchKnowledge_PathParity(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping knowledge search parity test")
	}
	ctx := context.Background()

	cfg := DefaultConfig(os.Getenv("DATABASE_URL"))
	cfg.EmbeddingDim = 3
	b, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	if !b.HasPgvector() {
		t.Skip("pgvector not installed — parity needs both paths live")
	}

	const orgID = "parity-org"
	const docID = "parity-doc"
	db := b.DB()
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id = $1`, docID)
		_, _ = db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID)
	})
	if _, err := db.ExecContext(ctx, `
		INSERT INTO knowledge_documents (id, org_id, filename) VALUES ($1, $2, 'parity.txt')
		ON CONFLICT (id) DO NOTHING`, docID, orgID); err != nil {
		t.Fatalf("seed doc: %v", err)
	}

	// Query is the +x axis. Similarities: near ~1.00, mid ~0.71,
	// mid-low ~0.30 (inside the old disagreement window), far 0.00.
	seed := []struct {
		id  string
		emb []float32
	}{
		{"p-near", []float32{1, 0.05, 0}},
		{"p-mid", []float32{0.7, 0.7, 0}},
		{"p-mid-low", []float32{0.3, 0.95, 0}},
		{"p-far", []float32{0, 1, 0}},
	}
	for _, c := range seed {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO knowledge_chunks (id, doc_id, content, embedding, embedding_vec)
			VALUES ($1, $2, $3, $4::jsonb, $5::vector)
			ON CONFLICT (id) DO UPDATE
			  SET embedding = EXCLUDED.embedding, embedding_vec = EXCLUDED.embedding_vec`,
			c.id, docID, c.id, FormatVector(c.emb), FormatVector(c.emb)); err != nil {
			t.Fatalf("seed chunk %s: %v", c.id, err)
		}
	}

	query := []float32{1, 0, 0}
	vec, err := b.searchKnowledgeVector(ctx, orgID, query, 10)
	if err != nil {
		t.Fatalf("searchKnowledgeVector: %v", err)
	}
	gos, err := b.searchKnowledgeGo(ctx, orgID, query, 10)
	if err != nil {
		t.Fatalf("searchKnowledgeGo: %v", err)
	}

	vecIDs := make([]string, 0, len(vec))
	for _, c := range vec {
		vecIDs = append(vecIDs, c.ID)
	}
	goIDs := make([]string, 0, len(gos))
	for _, c := range gos {
		goIDs = append(goIDs, c.ID)
	}

	if len(vecIDs) != len(goIDs) {
		t.Fatalf("path disagreement: pgvector returned %v, Go returned %v", vecIDs, goIDs)
	}
	for i := range vecIDs {
		if vecIDs[i] != goIDs[i] {
			t.Fatalf("path disagreement at %d: pgvector %v, Go %v", i, vecIDs, goIDs)
		}
	}

	// And both must apply the relevance floor: the sub-threshold chunks are out.
	for _, id := range vecIDs {
		if id == "p-far" || id == "p-mid-low" {
			t.Errorf("%s is below the relevance floor but was returned", id)
		}
	}
	if len(vecIDs) != 2 {
		t.Errorf("expected 2 chunks above the floor, got %v", vecIDs)
	}
}
