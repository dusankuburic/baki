package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// ---- Knowledge base operations ----

// SaveKnowledgeDocument inserts one document row. The write runs in the request RLS transaction when one
// is on ctx, otherwise in a short RLS transaction opened here as the caller — a bare pool connection would
// leave app_rls_active() false, short-circuiting the WITH CHECK policy to "allow".
func (b *PostgresStorageBackend) SaveKnowledgeDocument(ctx context.Context, userID string, doc *interfaces.KnowledgeDocument) error {
	return b.withTxAs(ctx, userID, func(_ context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO knowledge_documents (id, org_id, filename) VALUES ($1, $2, $3)`,
			doc.ID, doc.OrgID, doc.Filename)
		return err
	})
}

// DeleteKnowledgeDocument removes one document row under the same RLS discipline as SaveKnowledgeDocument.
func (b *PostgresStorageBackend) DeleteKnowledgeDocument(ctx context.Context, userID, orgID, id string) error {
	return b.withTxAs(ctx, userID, func(_ context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1 AND org_id = $2`, id, orgID)
		return err
	})
}

// DeleteKnowledgeDocumentByName gives re-upload REPLACE semantics: chunks cascade with the
// document rows, so the stale version stops influencing retrieval the moment the new upload begins.
func (b *PostgresStorageBackend) DeleteKnowledgeDocumentByName(ctx context.Context, orgID, filename string) error {
	_, err := b.query(ctx).ExecContext(ctx, `DELETE FROM knowledge_documents WHERE org_id = $1 AND filename = $2`, orgID, filename)
	return err
}

// listKnowledgeDocumentsSQL is the ListKnowledgeDocuments statement: the org's documents, newest first,
// cut to the page FIRST, then each returned document's chunk counts by one keyed LATERAL probe (the LEFT
// JOIN … GROUP BY … LIMIT form it replaced aggregated every chunk of the org before the LIMIT could drop a
// single document). Two literal statements: the embedding_vec column exists only where pgvector is installed.
func listKnowledgeDocumentsSQL(pgvector bool) string {
	if pgvector {
		return `
		SELECT d.id, d.org_id, d.filename, d.created_at, c.chunk_count, c.indexed_count
		FROM (SELECT id, org_id, filename, created_at FROM knowledge_documents
		      WHERE org_id = $1
		      ORDER BY created_at DESC
		      LIMIT CASE WHEN $2 > 0 THEN $2 ELSE NULL END
		      OFFSET GREATEST($3, 0)) d
		CROSS JOIN LATERAL (
			SELECT COUNT(*)::int AS chunk_count,
			       COUNT(*) FILTER (WHERE c.embedding_vec IS NOT NULL)::int AS indexed_count
			FROM knowledge_chunks c
			WHERE c.org_id = d.org_id AND c.doc_id = d.id) c
		ORDER BY d.created_at DESC`
	}
	return `
		SELECT d.id, d.org_id, d.filename, d.created_at, c.chunk_count, c.indexed_count
		FROM (SELECT id, org_id, filename, created_at FROM knowledge_documents
		      WHERE org_id = $1
		      ORDER BY created_at DESC
		      LIMIT CASE WHEN $2 > 0 THEN $2 ELSE NULL END
		      OFFSET GREATEST($3, 0)) d
		CROSS JOIN LATERAL (
			SELECT COUNT(*)::int AS chunk_count,
			       0::int AS indexed_count
			FROM knowledge_chunks c
			WHERE c.org_id = d.org_id AND c.doc_id = d.id) c
		ORDER BY d.created_at DESC`
}

// ListKnowledgeDocuments joins per-doc chunk counts and vector-indexed counts: a doc with chunks
// but 0 indexed is stranded by an embedding switch — the UI surfaces it and offers re-index.
func (b *PostgresStorageBackend) ListKnowledgeDocuments(ctx context.Context, orgID string, limit, offset int) ([]*interfaces.KnowledgeDocument, error) {
	rows, err := b.query(ctx).QueryContext(ctx, listKnowledgeDocumentsSQL(b.hasPgvector), orgID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []*interfaces.KnowledgeDocument
	for rows.Next() {
		var d interfaces.KnowledgeDocument
		if err := rows.Scan(&d.ID, &d.OrgID, &d.Filename, &d.CreatedAt, &d.ChunkCount, &d.VectorIndexed); err != nil {
			return nil, err
		}
		docs = append(docs, &d)
	}
	return docs, rows.Err()
}

// knowledgeDocumentIDsByFilenameSQL is the ListKnowledgeDocumentIDsByFilename statement: the org's count
// (index-only walk) LEFT JOINed to the documents named $2 — the LEFT JOIN keeps the count row when no
// document carries the name, so the result is never empty (≥1 row, NULL id).
const knowledgeDocumentIDsByFilenameSQL = `
		SELECT d.id, t.total
		FROM (SELECT COUNT(*)::int AS total FROM knowledge_documents WHERE org_id = $1) t
		LEFT JOIN knowledge_documents d ON d.org_id = $1 AND d.filename = $2
		ORDER BY d.created_at, d.id`

// ListKnowledgeDocumentIDsByFilename returns the ids of orgID's documents named filename, oldest
// first, and the org's total document count — what a re-upload needs without a full listing.
func (b *PostgresStorageBackend) ListKnowledgeDocumentIDsByFilename(ctx context.Context, orgID, filename string) ([]string, int, error) {
	rows, err := b.query(ctx).QueryContext(ctx, knowledgeDocumentIDsByFilenameSQL, orgID, filename)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var ids []string
	total := 0
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id, &total); err != nil {
			return nil, 0, err
		}
		if id.Valid {
			ids = append(ids, id.String)
		}
	}
	return ids, total, rows.Err()
}

// SaveKnowledgeChunks inserts chunk rows. knowledge_chunks has RLS policies; the inserts MUST run in a
// transaction with app.current_user_id set (withTxAs) — otherwise the WITH CHECK policy short-circuits to
// "allow", letting any authenticated caller write chunks for arbitrary org_id values. Reuse the middleware's tx when present.
func (b *PostgresStorageBackend) SaveKnowledgeChunks(ctx context.Context, userID string, chunks []interfaces.KnowledgeChunk) error {
	if len(chunks) == 0 {
		return nil
	}

	runInserts := func(tx DBTX) error {
		// A caller that did not set org_id (v28) gets the parent document's org resolved here — the
		// policy and every org-scoped read key on it; the v28 BEFORE INSERT trigger is the same rule at the database.
		if err := b.fillKnowledgeChunkOrgs(ctx, tx, chunks); err != nil {
			return err
		}
		// embedding (JSONB) is always written (portability + pgvector backfill source); embedding_vec
		// only when pgvector is active AND the dimension matches the configured contract — mismatched
		// chunks stay NULL so the HNSW index never sees an incomparable vector. Multi-row INSERTs.
		const rowsPerStmt = 2000
		for start := 0; start < len(chunks); start += rowsPerStmt {
			end := min(start+rowsPerStmt, len(chunks))
			batch := chunks[start:end]

			var sb strings.Builder
			args := make([]any, 0, len(batch)*6)
			if b.hasPgvector {
				sb.WriteString(`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding, embedding_vec) VALUES `)
				for i, c := range batch {
					embJSON, _ := json.Marshal(c.Embedding)
					var vec any
					if len(c.Embedding) == b.embeddingDim {
						vec = FormatVector(c.Embedding)
					} else if len(c.Embedding) > 0 {
						logger.Warn("knowledge chunk embedding dimension differs from configured; excluded from vector index",
							"chunk_id", c.ID, "got_dim", len(c.Embedding), "configured_dim", b.embeddingDim)
					}
					if i > 0 {
						sb.WriteByte(',')
					}
					base := i*6 + 1
					fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d::vector)", base, base+1, base+2, base+3, base+4, base+5)
					args = append(args, c.ID, c.DocID, c.OrgID, c.Content, embJSON, vec)
				}
			} else {
				sb.WriteString(`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES `)
				for i, c := range batch {
					embJSON, _ := json.Marshal(c.Embedding)
					if i > 0 {
						sb.WriteByte(',')
					}
					base := i*5 + 1
					fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4)
					args = append(args, c.ID, c.DocID, c.OrgID, c.Content, embJSON)
				}
			}
			if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
				return fmt.Errorf("save knowledge chunks rows %d..%d: %w", start, end, err)
			}
		}
		return nil
	}
	return b.withTxAs(ctx, userID, func(_ context.Context, tx *sql.Tx) error { return runInserts(tx) })
}

// fillKnowledgeChunkOrgs sets OrgID on every chunk that lacks one from its document, with one read for all
// distinct documents. A chunk whose document is not visible (or absent) keeps the blank org and is then
// refused by the WITH CHECK; runs on tx so it sees same-transaction writes.
func (b *PostgresStorageBackend) fillKnowledgeChunkOrgs(ctx context.Context, tx DBTX, chunks []interfaces.KnowledgeChunk) error {
	var docIDs []string
	seen := map[string]bool{}
	for _, c := range chunks {
		if c.OrgID == "" && !seen[c.DocID] {
			seen[c.DocID] = true
			docIDs = append(docIDs, c.DocID)
		}
	}
	if len(docIDs) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, org_id FROM knowledge_documents WHERE id = ANY($1)`, docIDs)
	if err != nil {
		return fmt.Errorf("save knowledge chunks: resolve document orgs: %w", err)
	}
	defer rows.Close()
	orgByDoc := make(map[string]string, len(docIDs))
	for rows.Next() {
		var id, org string
		if err := rows.Scan(&id, &org); err != nil {
			return fmt.Errorf("save knowledge chunks: resolve document orgs: %w", err)
		}
		orgByDoc[id] = org
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("save knowledge chunks: resolve document orgs: %w", err)
	}
	for i := range chunks {
		if chunks[i].OrgID == "" {
			chunks[i].OrgID = orgByDoc[chunks[i].DocID]
		}
	}
	return nil
}

// knowledgeMinSimilarity is the relevance floor for retrieval; knowledgeMaxCosineDistance is the identical
// cutoff in pgvector's units (<=> is cosine DISTANCE = 1 - similarity) — together they keep the paths in step.
const (
	knowledgeMinSimilarity     float32 = 0.5
	knowledgeMaxCosineDistance         = 1 - float64(knowledgeMinSimilarity)
)

func (b *PostgresStorageBackend) SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	// pgvector path: push the similarity ordering + LIMIT into the database; only same-dimension
	// chunks are indexed, and <=> is cosine distance, so ORDER BY it ASC returns the most similar.
	if b.hasPgvector && len(queryEmbedding) == b.embeddingDim {
		return b.searchKnowledgeVector(ctx, orgID, queryEmbedding, limit)
	}
	// Go-side fallback: no pgvector, or the query dimension doesn't match the index — loads a
	// deterministic capped sample and ranks in-process for portability.
	return b.searchKnowledgeGo(ctx, orgID, queryEmbedding, limit)
}

// searchKnowledgeVector runs the server-side similarity search. The relevance cutoff MUST match the Go
// fallback's — an optimization that changes results is a bug (the old `<=> < 1.0` and Go `similarity >= 0.5`
// disagreed); knowledgeMaxCosineDistance is the single definition. The SELECT omits c.embedding (the
// ~15-30KB JSONB copy). With a configured width AND pgvector 0.8+ iterative scans the statement is
// knowledgeVectorSearchSQL: the ORDER BY is on the expression the HNSW index is built on, so the planner
// walks the index (runs in a transaction because the hnsw settings are SET LOCAL). The index is multi-tenant
// — without an iterative scan HNSW yields hnsw.ef_search (40) GLOBAL candidates and stops, starving a small
// tenant, so on pgvector < 0.8 the index is not used at all; with it, the scan keeps walking until LIMIT
// rows pass the org filter, bounded by hnsw.max_scan_tuples (raised here so a small tenant in a large
// corpus is not cut short).
func (b *PostgresStorageBackend) searchKnowledgeVector(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	stmt, indexed := b.knowledgeVectorStatement()
	if !indexed {
		rows, err := b.query(ctx).QueryContext(ctx, stmt, orgID, FormatVector(queryEmbedding), limit, knowledgeMaxCosineDistance, b.embeddingDim)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanKnowledgeHits(rows)
	}
	var chunks []interfaces.KnowledgeChunk
	err := b.withTx(ctx, func(_ context.Context, tx *sql.Tx) error {
		for _, stmt := range []string{
			`SET LOCAL hnsw.iterative_scan = relaxed_order`,
			fmt.Sprintf(`SET LOCAL hnsw.max_scan_tuples = %d`, hnswMaxScanTuples),
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
		rows, err := tx.QueryContext(ctx, stmt, orgID, FormatVector(queryEmbedding), limit, knowledgeMaxCosineDistance)
		if err != nil {
			return err
		}
		defer rows.Close()
		chunks, err = scanKnowledgeHits(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return chunks, nil
}

// knowledgeVectorStatement picks the statement searchKnowledgeVector runs: the indexed form only with a
// width to cast to AND an iterable HNSW scan (a correctness condition, not a tuning one).
func (b *PostgresStorageBackend) knowledgeVectorStatement() (stmt string, indexed bool) {
	if b.embeddingDim <= 0 || !b.hnswIterativeScan {
		return knowledgeVectorSearchExactSQL, false
	}
	return knowledgeVectorSearchSQL(b.embeddingDim), true
}

// hnswMaxScanTuples is the hnsw.max_scan_tuples the indexed search sets: how many index tuples an iterative
// scan may visit before giving up on LIMIT rows passing the org filter (default 20000).
const hnswMaxScanTuples = 100000

// knowledgeVectorSearchExactSQL is the exact-scan form (no index expression to match), used with no configured
// width or a non-iterating pgvector. Args: $1 org, $2 query vector, $3 limit, $4 max cosine distance, $5 width (0 = any).
const knowledgeVectorSearchExactSQL = `
		SELECT nearest.id, nearest.doc_id, nearest.content, d.filename
		FROM (
			SELECT c.id, c.doc_id, c.content,
			       c.embedding_vec <=> $2::vector AS dist
			FROM knowledge_chunks c
			WHERE c.org_id = $1
			  AND c.embedding_vec IS NOT NULL
			  AND ($5 <= 0 OR vector_dims(c.embedding_vec) = $5)
			  AND c.embedding_vec <=> $2::vector <= $4
			ORDER BY dist
			LIMIT $3
		) nearest
		JOIN knowledge_documents d ON d.id = nearest.doc_id
		ORDER BY nearest.dist`

// knowledgeVectorSearchSQL is the indexed form for width dims (same args minus $5): the inner query is exactly
// what the HNSW index serves — the cast expression it is built on, its partial predicate verbatim, ORDER BY on
// the distance computed once, the LIMIT. The cutoff is applied OUTSIDE the inner WHERE (the k nearest filtered
// to the close-enough ones equals the nearest k of the close-enough ones, and keeping it out lets the LIMIT
// push into the index scan); the document join is outside the LIMIT too.
func knowledgeVectorSearchSQL(dims int) string {
	return fmt.Sprintf(`
		SELECT nearest.id, nearest.doc_id, nearest.content, d.filename
		FROM (
			SELECT c.id, c.doc_id, c.content,
			       c.embedding_vec::vector(%[1]d) <=> $2::vector(%[1]d) AS dist
			FROM knowledge_chunks c
			WHERE c.org_id = $1
			  AND vector_dims(c.embedding_vec) = %[1]d
			ORDER BY dist
			LIMIT $3
		) nearest
		JOIN knowledge_documents d ON d.id = nearest.doc_id
		WHERE nearest.dist <= $4
		ORDER BY nearest.dist`, dims)
}

// scanKnowledgeHits reads (id, doc_id, content, filename) rows.
func scanKnowledgeHits(rows *sql.Rows) ([]interfaces.KnowledgeChunk, error) {
	var chunks []interfaces.KnowledgeChunk
	for rows.Next() {
		var c interfaces.KnowledgeChunk
		if err := rows.Scan(&c.ID, &c.DocID, &c.Content, &c.Filename); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate knowledge chunks: %w", err)
	}
	return chunks, nil
}

// searchKnowledgeGo is the portability fallback: load a deterministic capped sample and rank in-process.
func (b *PostgresStorageBackend) searchKnowledgeGo(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	// Cap the DB load to avoid OOM; cosine sort and truncation stay in Go. ORDER BY doc_id, id makes
	// the 500-chunk sample deterministic. The jsonb_array_length filter drops width-mismatched chunks
	// BEFORE transfer (cosine is undefined across widths; a 1536-dim embedding is ~15-30 KB of JSON).
	const maxChunks = 500
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT c.id, c.doc_id, c.content, c.embedding, d.filename
		FROM knowledge_chunks c
		JOIN knowledge_documents d ON d.id = c.doc_id
		WHERE c.org_id = $1
		  AND jsonb_array_length(c.embedding) = $2
		ORDER BY c.doc_id, c.id
		LIMIT $3`, orgID, len(queryEmbedding), maxChunks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []interfaces.KnowledgeChunk
	for rows.Next() {
		var c interfaces.KnowledgeChunk
		var embJSON []byte
		if err := rows.Scan(&c.ID, &c.DocID, &c.Content, &embJSON, &c.Filename); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(embJSON, &c.Embedding); err != nil {
			logger.Warn("corrupt embedding JSON", "chunk_id", c.ID, "error", err)
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate knowledge chunks: %w", err)
	}
	// The capped sample is deterministic but ARBITRARY: beyond maxChunks the rest are permanently
	// invisible — surface it so ops knows the fallback is lossy here (install pgvector / re-index).
	if len(chunks) >= maxChunks {
		logger.Warn("knowledge search fallback capped — chunks beyond the sample are unsearchable",
			"org_id", orgID, "sampled", len(chunks))
		metrics.RecordRAGFallbackCapped()
	}

	return rankKnowledgeChunks(orgID, chunks, queryEmbedding, limit)
}

// ListKnowledgeChunkContents returns one page of chunks (no embeddings) strictly after the (doc_id, id) cursor,
// for re-indexing: the embedding provider changed, but the chunked TEXT is still the source of truth.
func (b *PostgresStorageBackend) ListKnowledgeChunkContents(ctx context.Context, orgID string, after interfaces.KnowledgeChunkCursor, limit int) ([]interfaces.KnowledgeChunk, error) {
	const maxPage = 2000
	if limit <= 0 || limit > maxPage {
		limit = maxPage
	}
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT c.id, c.doc_id, c.content
		FROM knowledge_chunks c
		WHERE c.org_id = $1
		  AND (c.doc_id, c.id) > ($2::text, $3::text)
		ORDER BY c.doc_id, c.id
		LIMIT $4`, orgID, after.DocID, after.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []interfaces.KnowledgeChunk
	for rows.Next() {
		var c interfaces.KnowledgeChunk
		if err := rows.Scan(&c.ID, &c.DocID, &c.Content); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// SampleKnowledgeChunkEmbeddings returns up to n of the org's chunks with their stored embeddings — ONE per
// document — for the RAG embedding-model mismatch guard (DISTINCT ON spreads the sample over the corpus; "the
// n lowest ids" would be a fixed subset a partially converted corpus can hide from). With pgvector, each
// document's indexed chunk is picked first; the CASE-form jsonb guard keeps rows without an embedding array
// out (jsonb_array_length would error on a scalar, and Postgres need not short-circuit an AND).
func (b *PostgresStorageBackend) SampleKnowledgeChunkEmbeddings(ctx context.Context, orgID string, n int) ([]interfaces.KnowledgeChunk, error) {
	if n <= 0 {
		return nil, nil
	}
	// Two literal statements (not a spliced ORDER BY): embedding_vec exists only with pgvector.
	query := `
		SELECT id, doc_id, content, embedding FROM (
			SELECT DISTINCT ON (c.doc_id) c.id, c.doc_id, c.content, c.embedding
			FROM knowledge_chunks c
			WHERE c.org_id = $1
			  AND CASE WHEN jsonb_typeof(c.embedding) = 'array'
			           THEN jsonb_array_length(c.embedding) ELSE 0 END > 0
			ORDER BY c.doc_id, c.id
		) s
		ORDER BY doc_id
		LIMIT $2`
	if b.hasPgvector {
		query = `
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
	}
	rows, err := b.query(ctx).QueryContext(ctx, query, orgID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []interfaces.KnowledgeChunk
	for rows.Next() {
		var c interfaces.KnowledgeChunk
		var embJSON []byte
		if err := rows.Scan(&c.ID, &c.DocID, &c.Content, &embJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(embJSON, &c.Embedding); err != nil {
			logger.Warn("corrupt embedding JSON", "chunk_id", c.ID, "error", err)
			continue
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// SampleKnowledgeChunkEmbeddingsUpTo returns up to n of the org's chunks at or before the (doc_id, id) cursor,
// nearest first, with stored embeddings — the chunks a resumed re-index's previous request converted (rag.reindexGuard).
func (b *PostgresStorageBackend) SampleKnowledgeChunkEmbeddingsUpTo(ctx context.Context, orgID string, upTo interfaces.KnowledgeChunkCursor, n int) ([]interfaces.KnowledgeChunk, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT c.id, c.doc_id, c.content, c.embedding
		FROM knowledge_chunks c
		WHERE c.org_id = $1
		  AND (c.doc_id, c.id) <= ($2::text, $3::text)
		  AND CASE WHEN jsonb_typeof(c.embedding) = 'array'
		           THEN jsonb_array_length(c.embedding) ELSE 0 END > 0
		ORDER BY c.doc_id DESC, c.id DESC
		LIMIT $4`, orgID, upTo.DocID, upTo.ID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []interfaces.KnowledgeChunk
	for rows.Next() {
		var c interfaces.KnowledgeChunk
		var embJSON []byte
		if err := rows.Scan(&c.ID, &c.DocID, &c.Content, &embJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(embJSON, &c.Embedding); err != nil {
			logger.Warn("corrupt embedding JSON", "chunk_id", c.ID, "error", err)
			continue
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// UpdateKnowledgeChunkEmbeddings replaces chunk embeddings in place for re-indexing (RLS-scoped like
// SaveKnowledgeChunks); embedding_vec refreshes only for vectors matching the configured dimension. The
// re-index route runs outside the request-long RLS transaction, so each call — one re-index page — opens and
// commits its own short RLS tx as the calling admin: a later page's failure keeps the pages before it.
func (b *PostgresStorageBackend) UpdateKnowledgeChunkEmbeddings(ctx context.Context, userID string, chunks []interfaces.KnowledgeChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	runUpdates := func(tx DBTX) error {
		const perStmt = 200
		for start := 0; start < len(chunks); start += perStmt {
			end := min(start+perStmt, len(chunks))
			stmt, args := buildKnowledgeEmbeddingUpdate(chunks[start:end], b.hasPgvector, b.embeddingDim)
			if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
				return fmt.Errorf("update knowledge chunk embeddings: %w", err)
			}
		}
		return nil
	}
	return b.withTxAs(ctx, userID, func(_ context.Context, tx *sql.Tx) error { return runUpdates(tx) })
}

// buildKnowledgeEmbeddingUpdate renders one multi-row UPDATE ... FROM (VALUES ...) statement (200 rows per
// statement). knowledge_chunks.id is TEXT and Postgres has no implicit text/uuid cast — the explicit ::text
// keeps the VALUES column typed so a NULL vector still plans (vec is NULL for vectors whose width is not embeddingDim).
func buildKnowledgeEmbeddingUpdate(batch []interfaces.KnowledgeChunk, hasPgvector bool, embeddingDim int) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, len(batch)*3)
	if hasPgvector {
		sb.WriteString(`UPDATE knowledge_chunks AS c SET embedding = v.emb, embedding_vec = v.vec
			FROM (VALUES `)
		for i, c := range batch {
			embJSON, _ := json.Marshal(c.Embedding)
			var vec any
			if len(c.Embedding) == embeddingDim {
				vec = FormatVector(c.Embedding)
			}
			if i > 0 {
				sb.WriteByte(',')
			}
			base := i*3 + 1
			fmt.Fprintf(&sb, "($%d::text, $%d::jsonb, $%d::vector)", base, base+1, base+2)
			args = append(args, c.ID, embJSON, vec)
		}
		sb.WriteString(`) AS v(id, emb, vec) WHERE c.id = v.id`)
		return sb.String(), args
	}
	sb.WriteString(`UPDATE knowledge_chunks AS c SET embedding = v.emb
		FROM (VALUES `)
	for i, c := range batch {
		embJSON, _ := json.Marshal(c.Embedding)
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*2 + 1
		fmt.Fprintf(&sb, "($%d::text, $%d::jsonb)", base, base+1)
		args = append(args, c.ID, embJSON)
	}
	sb.WriteString(`) AS v(id, emb) WHERE c.id = v.id`)
	return sb.String(), args
}

// CountKnowledgeChunks reports the org's total chunks and how many sit in the pgvector index at the
// configured dimension — total>0 with 0 indexed (pgvector active) means an embedding switch stranded the corpus.
func (b *PostgresStorageBackend) CountKnowledgeChunks(ctx context.Context, orgID string) (int, int, error) {
	var total, indexed int
	query := `
		SELECT COUNT(c.id), 0
		FROM knowledge_chunks c
		WHERE c.org_id = $1`
	if b.hasPgvector {
		query = `
		SELECT COUNT(c.id), COUNT(c.id) FILTER (WHERE c.embedding_vec IS NOT NULL)
		FROM knowledge_chunks c
		WHERE c.org_id = $1`
	}
	err := b.query(ctx).QueryRowContext(ctx, query, orgID).Scan(&total, &indexed)
	return total, indexed, err
}

// rankKnowledgeChunks scores chunks against queryEmbedding by cosine similarity and returns the top `limit`,
// highest first. Width-mismatched chunks are skipped rather than scored 0; if nothing is comparable, it fails
// loudly so the caller re-indexes instead of receiving silently-irrelevant results.
func rankKnowledgeChunks(orgID string, chunks []interfaces.KnowledgeChunk, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	// Score each chunk once, then sort — scoring inside the comparator would recompute O(n log n) times.
	qDim := len(queryEmbedding)
	type scored struct {
		chunk interfaces.KnowledgeChunk
		sim   float32
	}
	scoredChunks := make([]scored, 0, len(chunks))
	mismatched := 0
	for _, c := range chunks {
		if len(c.Embedding) != qDim {
			mismatched++
			continue
		}
		scoredChunks = append(scoredChunks, scored{chunk: c, sim: cosineSimilarity(c.Embedding, queryEmbedding)})
	}
	if len(scoredChunks) == 0 && mismatched > 0 {
		return nil, fmt.Errorf("knowledge search embedding dimension mismatch: query has %d dims but all %d stored chunks differ — re-index the knowledge base after changing the embedding provider", qDim, mismatched)
	}
	if mismatched > 0 {
		logger.Warn("knowledge search skipped chunks with mismatched embedding dimension",
			"org_id", orgID, "query_dim", qDim, "skipped", mismatched, "scored", len(scoredChunks))
	}
	sort.Slice(scoredChunks, func(i, j int) bool {
		return scoredChunks[i].sim > scoredChunks[j].sim
	})

	// Filter by minimum similarity — the SAME cutoff the pgvector path applies as cosine distance
	// (knowledgeMaxCosineDistance); keep the two in step or the extension's presence changes results.
	cutoff := 0
	for cutoff < len(scoredChunks) && scoredChunks[cutoff].sim >= knowledgeMinSimilarity {
		cutoff++
	}
	scoredChunks = scoredChunks[:cutoff]

	if len(scoredChunks) > limit {
		scoredChunks = scoredChunks[:limit]
	}
	result := make([]interfaces.KnowledgeChunk, len(scoredChunks))
	for i, sc := range scoredChunks {
		result[i] = sc.chunk
	}
	return result, nil
}

func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float32
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}

// FormatVector renders a float32 embedding as the pgvector text literal (`'[0.1,0.2,…]'`) for INSERT
// (embedding_vec) and the <=> query operand. A nil/empty slice yields '[]' which pgvector rejects.
func FormatVector(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
