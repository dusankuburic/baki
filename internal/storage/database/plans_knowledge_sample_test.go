package database

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// The embedding-model guard samples one chunk per document (SampleKnowledgeChunkEmbeddings). With pgvector, whether a
// chunk has an embedding is answered by embedding_vec, so the statement must not read every chunk's JSON embedding
// (15-30 KB each, TOASTed) to pick the few it returns. DATABASE_URL-gated, on a private database with pgvector.

// oldSampleKnowledgeChunkEmbeddingsPgvectorSQL is the pgvector statement before v49 (the oracle): its guard reads the
// JSON embedding of every chunk of the organisation.
const oldSampleKnowledgeChunkEmbeddingsPgvectorSQL = `
		SELECT id, doc_id, content, embedding FROM (
			SELECT DISTINCT ON (c.doc_id) c.id, c.doc_id, c.content, c.embedding,
			       (c.embedding_vec IS NULL) AS unindexed
			FROM knowledge_chunks c
			WHERE c.org_id = $1
			  AND CASE WHEN jsonb_typeof(c.embedding) = 'array'
			           THEN jsonb_array_length(c.embedding) ELSE 0 END > 0
			ORDER BY c.doc_id, (c.embedding_vec IS NULL), c.id
		) s
		ORDER BY unindexed, doc_id
		LIMIT $2`

// explainBufferBlocks runs EXPLAIN (ANALYZE, BUFFERS) of q on the pool and returns the shared blocks the statement
// touched (hit plus read, the top node's totals, which include the TOAST pages a detoast reads).
func explainBufferBlocks(t *testing.T, b *PostgresStorageBackend, q string, args ...any) int {
	t.Helper()
	var raw string
	if err := b.db.QueryRowContext(context.Background(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+q, args...).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN ANALYZE: %v\n%s", err, q)
	}
	var top []struct {
		Plan struct {
			Hit  int `json:"Shared Hit Blocks"`
			Read int `json:"Shared Read Blocks"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &top); err != nil || len(top) == 0 {
		t.Fatalf("parse explain json: %v\n%s", err, raw)
	}
	return top[0].Plan.Hit + top[0].Plan.Read
}

// TestKnowledgeSample_ReadsOnlyTheSampledEmbeddings: over 150 documents of three 1536-wide chunks, the sample returns
// what the pre-v49 statement returned (also for documents whose chunks are unindexed, empty or not an array), and
// touches a fraction of the blocks: the old one detoasted every chunk's JSON embedding to test it.
func TestKnowledgeSample_ReadsOnlyTheSampledEmbeddings(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	if !b.hasPgvector {
		t.Skip("pgvector not installed: the JSON path is the only one")
	}
	ctx := context.Background()
	const org = "ks-org"
	for _, s := range []string{
		`INSERT INTO organisations (id, name, owner_id) VALUES ('` + org + `', 'Org', '')`,
		`INSERT INTO knowledge_documents (id, org_id, filename)
			SELECT 'ks-doc-' || lpad(g::text, 3, '0'), '` + org + `', 'f' || g || '.md' FROM generate_series(1, 150) g`,
		// Every chunk embedded and indexed (the JSON copy is what the release before also wrote).
		`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding, embedding_vec)
			SELECT 'ks-c-' || lpad(g::text, 3, '0') || '-' || k, 'ks-doc-' || lpad(g::text, 3, '0'), '` + org + `', 'text ' || g || '-' || k, e.j, (e.j::text)::vector
			FROM generate_series(1, 150) g, generate_series(1, 3) k,
			LATERAL (SELECT jsonb_agg(round(random()::numeric, 6)) AS j FROM generate_series(1, 1536) WHERE g > 0 AND k > 0) e`,
		// Documents the guard must still see or skip by their JSON: an unindexed embedding (another width), an empty
		// array, a scalar.
		`INSERT INTO knowledge_documents (id, org_id, filename) VALUES
			('ks-mixed-a', '` + org + `', 'a.md'), ('ks-mixed-b', '` + org + `', 'b.md'), ('ks-mixed-c', '` + org + `', 'c.md')`,
		`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES
			('ks-m-a-1', 'ks-mixed-a', '` + org + `', 'a1', '[0.1, 0.2]'),
			('ks-m-b-1', 'ks-mixed-b', '` + org + `', 'b1', '[]'),
			('ks-m-b-2', 'ks-mixed-b', '` + org + `', 'b2', '5'),
			('ks-m-b-3', 'ks-mixed-b', '` + org + `', 'b3', '[0.3, 0.4, 0.5]'),
			('ks-m-c-1', 'ks-mixed-c', '` + org + `', 'c1', '[]')`,
		`ANALYZE knowledge_documents, knowledge_chunks`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}

	ids := func(q string, n int) []string {
		t.Helper()
		rows, err := b.db.QueryContext(ctx, q, org, n)
		if err != nil {
			t.Fatalf("sample: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id, doc, content string
			var emb []byte
			if err := rows.Scan(&id, &doc, &content, &emb); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, n := range []int{1, 3, 1000} {
		want, got := ids(oldSampleKnowledgeChunkEmbeddingsPgvectorSQL, n), ids(sampleKnowledgeChunkEmbeddingsSQL(true), n)
		if !slices.Equal(got, want) {
			t.Errorf("n=%d: the sample is %v, the pre-v49 statement's %v", n, got, want)
		}
	}
	if all := ids(sampleKnowledgeChunkEmbeddingsSQL(true), 1000); len(all) != 152 || !slices.Contains(all, "ks-m-a-1") || !slices.Contains(all, "ks-m-b-3") {
		t.Errorf("the full sample has %d chunks, want 152 (150 indexed documents, then a's and b's unindexed array)", len(all))
	}

	got, err := b.SampleKnowledgeChunkEmbeddings(ctx, org, 3)
	if err != nil || len(got) != 3 || len(got[0].Embedding) != 1536 || got[0].Content == "" {
		t.Fatalf("SampleKnowledgeChunkEmbeddings = %d chunks, %v; want 3 with their content and 1536-wide embeddings", len(got), err)
	}

	before := explainBufferBlocks(t, b, oldSampleKnowledgeChunkEmbeddingsPgvectorSQL, org, 3)
	after := explainBufferBlocks(t, b, sampleKnowledgeChunkEmbeddingsSQL(true), org, 3)
	t.Logf("shared blocks touched: pre-v49 %d, now %d", before, after)
	if after*4 > before {
		t.Errorf("the sample touches %d blocks, the pre-v49 statement %d: it still reads every chunk's JSON embedding", after, before)
	}
}
