package database

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// TestBuildKnowledgeEmbeddingUpdate_NoUUIDCast pins the re-index UPDATE's
// shape without a database: knowledge_chunks.id is TEXT, so the id
// parameter must be cast ::text — a ::uuid cast fails at plan time
// ("operator does not exist: text = uuid") and no re-index could ever
// commit. Both variants (with and without pgvector), 2 or 3 params per row,
// the VALUES alias, and a NULL vector for a width the index does not hold.
func TestBuildKnowledgeEmbeddingUpdate_NoUUIDCast(t *testing.T) {
	batch := []interfaces.KnowledgeChunk{
		{ID: "id-1", Embedding: []float32{1, 2, 3}},
		{ID: "id-2", Embedding: []float32{4, 5}}, // wrong width: no vector
	}
	for _, pgvector := range []bool{true, false} {
		t.Run(fmt.Sprintf("pgvector=%v", pgvector), func(t *testing.T) {
			stmt, args := buildKnowledgeEmbeddingUpdate(batch, pgvector, 3)
			if strings.Contains(stmt, "::uuid") {
				t.Fatalf("statement casts the TEXT id to uuid: %s", stmt)
			}
			if !strings.HasPrefix(stmt, "UPDATE knowledge_chunks AS c SET embedding = v.emb") || !strings.HasSuffix(stmt, "WHERE c.id = v.id") {
				t.Errorf("unexpected statement shape: %s", stmt)
			}
			perRow := 2
			if pgvector {
				perRow = 3
				if !strings.Contains(stmt, "embedding_vec = v.vec") || !strings.Contains(stmt, "AS v(id, emb, vec)") {
					t.Errorf("pgvector variant must refresh embedding_vec: %s", stmt)
				}
				if args[2] != "[1,2,3]" || args[5] != nil {
					t.Errorf("vector args = %v / %v, want the width-3 literal and NULL for the width-2 row", args[2], args[5])
				}
			} else {
				if strings.Contains(stmt, "embedding_vec") || !strings.Contains(stmt, "AS v(id, emb)") {
					t.Errorf("non-pgvector variant must not touch embedding_vec: %s", stmt)
				}
			}
			if len(args) != len(batch)*perRow {
				t.Fatalf("%d args, want %d", len(args), len(batch)*perRow)
			}
			for i := range batch {
				if got := strings.Count(stmt, fmt.Sprintf("($%d::text, $%d::jsonb", i*perRow+1, i*perRow+2)); got != 1 {
					t.Errorf("row %d: id/jsonb placeholders found %d times in %s", i, got, stmt)
				}
				if args[i*perRow] != batch[i].ID {
					t.Errorf("row %d: id arg = %v, want %q", i, args[i*perRow], batch[i].ID)
				}
			}
		})
	}
}

// newKnowledgeIntegrationBackend opens the DATABASE_URL backend (podman
// harness) with a 3-wide embedding column, or skips.
func newKnowledgeIntegrationBackend(t *testing.T) *PostgresStorageBackend {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping knowledge integration test")
	}
	cfg := DefaultConfig(dsn)
	cfg.EmbeddingDim = 3
	b, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// seedKnowledge inserts documents (id → org) and chunks (id, doc, embedding
// JSON) with raw SQL, removing them at cleanup.
func seedKnowledge(t *testing.T, b *PostgresStorageBackend, docs map[string]string, chunks []struct{ id, doc, emb string }) {
	t.Helper()
	ctx := context.Background()
	db := b.DB()
	t.Cleanup(func() {
		for doc := range docs {
			_, _ = db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id = $1`, doc)
			_, _ = db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, doc)
		}
	})
	for doc, org := range docs {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO knowledge_documents (id, org_id, filename) VALUES ($1, $2, $3)
			ON CONFLICT (id) DO NOTHING`, doc, org, doc+".txt"); err != nil {
			t.Fatalf("seed doc: %v", err)
		}
	}
	for _, c := range chunks {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO knowledge_chunks (id, doc_id, content, embedding)
			VALUES ($1, $2, $3, $4::jsonb)
			ON CONFLICT (id) DO UPDATE SET embedding = EXCLUDED.embedding`,
			c.id, c.doc, "text of "+c.id, c.emb); err != nil {
			t.Fatalf("seed chunk %s: %v", c.id, err)
		}
	}
}

// TestUpdateKnowledgeChunkEmbeddings_Postgres executes the re-index UPDATE
// against a live database: a TEXT id row is updated (the ::uuid cast made
// this fail at plan time), with and without the pgvector branch.
func TestUpdateKnowledgeChunkEmbeddings_Postgres(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	seedKnowledge(t, b, map[string]string{"reindex-doc": "reindex-org"}, []struct{ id, doc, emb string }{
		{"reindex-1", "reindex-doc", "[1,0,0]"},
		{"reindex-2", "reindex-doc", "[0,1,0]"},
	})
	readBack := func(id string) []float32 {
		var raw []byte
		if err := b.DB().QueryRowContext(ctx, `SELECT embedding FROM knowledge_chunks WHERE id = $1`, id).Scan(&raw); err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		var v []float32
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("decode %s: %v", id, err)
		}
		return v
	}

	detected := b.hasPgvector
	for _, pgvector := range []bool{detected, false} {
		b.hasPgvector = pgvector
		want := []float32{9, 9, 9}
		if !pgvector {
			want = []float32{8, 8, 8}
		}
		err := b.UpdateKnowledgeChunkEmbeddings(ctx, "", []interfaces.KnowledgeChunk{
			{ID: "reindex-1", DocID: "reindex-doc", Embedding: want},
			{ID: "reindex-2", DocID: "reindex-doc", Embedding: []float32{7, 7}}, // other width: JSON only
		})
		if err != nil {
			t.Fatalf("pgvector=%v: UpdateKnowledgeChunkEmbeddings: %v", pgvector, err)
		}
		if got := readBack("reindex-1"); len(got) != 3 || got[0] != want[0] {
			t.Errorf("pgvector=%v: stored embedding = %v, want %v", pgvector, got, want)
		}
		if got := readBack("reindex-2"); len(got) != 2 || got[0] != 7 {
			t.Errorf("pgvector=%v: other-width embedding = %v, want [7 7]", pgvector, got)
		}
	}
	b.hasPgvector = detected
	if detected {
		var indexed int
		if err := b.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_chunks WHERE doc_id = 'reindex-doc' AND embedding_vec IS NOT NULL`).Scan(&indexed); err != nil {
			t.Fatalf("count indexed: %v", err)
		}
		if indexed != 1 {
			t.Errorf("%d rows vector-indexed, want 1 (only the width-3 row)", indexed)
		}
	}
}

// TestSampleKnowledgeChunkEmbeddings_OnePerDocument: the mismatch-guard
// sample spreads over the corpus — one chunk per document — instead of the
// n lowest ids, which a partially converted corpus could hide from.
func TestSampleKnowledgeChunkEmbeddings_OnePerDocument(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	docs := map[string]string{}
	var chunks []struct{ id, doc, emb string }
	for d := range 3 {
		doc := fmt.Sprintf("sample-perdoc-%d", d)
		docs[doc] = "sample-perdoc-org"
		for i := range 5 {
			chunks = append(chunks, struct{ id, doc, emb string }{fmt.Sprintf("%s-c%d", doc, i), doc, "[1,0,0]"})
		}
	}
	seedKnowledge(t, b, docs, chunks)

	got, err := b.SampleKnowledgeChunkEmbeddings(ctx, "sample-perdoc-org", 3)
	if err != nil {
		t.Fatalf("SampleKnowledgeChunkEmbeddings: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c.DocID] {
			t.Errorf("document %s sampled twice", c.DocID)
		}
		seen[c.DocID] = true
		if len(c.Embedding) != 3 || c.Content != "text of "+c.ID {
			t.Errorf("sampled chunk %+v lacks its content or embedding", c)
		}
	}
	if len(seen) != 3 {
		t.Errorf("sampled %d distinct documents (%d chunks), want 3", len(seen), len(got))
	}
	if more, err := b.SampleKnowledgeChunkEmbeddings(ctx, "sample-perdoc-org", 10); err != nil || len(more) != 3 {
		t.Errorf("n=10 over 3 documents: %d chunks, %v; want 3 (one per document)", len(more), err)
	}
}

// TestKnowledgeCounts_WithoutPgvector: the document list and the chunk
// counts must not reference embedding_vec on a database without the
// extension (the column does not exist there). Exercised by flipping the
// detected flag off: the no-pgvector statements never name the column.
func TestKnowledgeCounts_WithoutPgvector(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	seedKnowledge(t, b, map[string]string{"nopgv-doc": "nopgv-org"}, []struct{ id, doc, emb string }{
		{"nopgv-1", "nopgv-doc", "[1,0,0]"},
		{"nopgv-2", "nopgv-doc", "[0,1,0]"},
	})
	detected := b.hasPgvector
	t.Cleanup(func() { b.hasPgvector = detected })

	for _, pgvector := range []bool{detected, false} {
		b.hasPgvector = pgvector
		total, indexed, err := b.CountKnowledgeChunks(ctx, "nopgv-org")
		if err != nil || total != 2 {
			t.Fatalf("pgvector=%v: CountKnowledgeChunks = %d/%d, %v; want total 2", pgvector, total, indexed, err)
		}
		if !pgvector && indexed != 0 {
			t.Errorf("without pgvector nothing is vector-indexed, got %d", indexed)
		}
		docs, err := b.ListKnowledgeDocuments(ctx, "nopgv-org", 0, 0)
		if err != nil || len(docs) != 1 || docs[0].ChunkCount != 2 {
			t.Fatalf("pgvector=%v: ListKnowledgeDocuments = %+v, %v; want one doc with 2 chunks", pgvector, docs, err)
		}
		if !pgvector && docs[0].VectorIndexed != 0 {
			t.Errorf("without pgvector VectorIndexed = %d, want 0", docs[0].VectorIndexed)
		}
	}
}
