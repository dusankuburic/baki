package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// ---- Knowledge base operations ----

// SaveKnowledgeDocument inserts one document row. The write runs in the request RLS transaction when one
// is on ctx, otherwise in a short RLS transaction opened here as the caller — a bare pool connection would
// leave app_rls_active() false, short-circuiting the WITH CHECK policy to "allow".
func (b *PostgresStorageBackend) SaveKnowledgeDocument(ctx context.Context, userID string, doc *interfaces.KnowledgeDocument) error {
	return b.withTxAs(ctx, userID, func(_ context.Context, tx *sql.Tx) error {
		// chunker_version 0 is "unsaid": the column's default (1), the geometry every document before the column had.
		// An unsaid state is ready (the column's default); an empty source text is none (NULL, v51).
		_, err := tx.ExecContext(ctx, `INSERT INTO knowledge_documents (id, org_id, filename, chunker_version, state, content)
			VALUES ($1, $2, $3, $4, COALESCE(NULLIF($5::text, ''), 'ready'), NULLIF($6::text, ''))`,
			doc.ID, doc.OrgID, doc.Filename, max(doc.ChunkerVersion, 1), doc.State, doc.Content)
		if isUnknownOrg(err, "knowledge_documents_org_id_fkey") {
			return interfaces.ErrOrgNotFound
		}
		return err
	})
}

// SaveKnowledgeDocumentWithChunks writes the document and its chunks in one transaction as the caller (the request's
// when one is on ctx; the upload route runs without one): the two methods it calls join it. Committed apart, a crash
// between them left a document with no chunks.
func (b *PostgresStorageBackend) SaveKnowledgeDocumentWithChunks(ctx context.Context, userID string, doc *interfaces.KnowledgeDocument, chunks []interfaces.KnowledgeChunk) error {
	return b.withTxAs(ctx, userID, func(txCtx context.Context, tx *sql.Tx) error {
		// The org's chunk-write lock before the document row, as a chunk write alone takes it before its rows: an
		// upload and a re-index page of one org queue for it in the same order.
		if err := lockKnowledgeOrgs(txCtx, tx, []interfaces.KnowledgeChunk{{OrgID: doc.OrgID}}); err != nil {
			return err
		}
		if err := b.SaveKnowledgeDocument(txCtx, userID, doc); err != nil {
			return err
		}
		return b.SaveKnowledgeChunks(txCtx, userID, chunks)
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
// single document). The id breaks created_at ties, so pages neither skip nor repeat a document, and the order is
// knowledge_documents_org_created_idx's (v49): the page is read in index order, not sorted. Two literal statements:
// the embedding_vec column exists only where pgvector is installed.
func listKnowledgeDocumentsSQL(pgvector bool) string {
	if pgvector {
		return `
		SELECT d.id, d.org_id, d.filename, d.created_at, d.chunker_version, d.state, d.has_source, c.chunk_count, c.indexed_count, c.pending_count
		FROM (SELECT id, org_id, filename, created_at, chunker_version, state, content IS NOT NULL AS has_source FROM knowledge_documents
		      WHERE org_id = $1
		      ORDER BY created_at DESC, id
		      LIMIT CASE WHEN $2 > 0 THEN $2 ELSE NULL END
		      OFFSET GREATEST($3, 0)) d
		CROSS JOIN LATERAL (
			SELECT COUNT(*)::int AS chunk_count,
			       COUNT(*) FILTER (WHERE c.embedding_vec IS NOT NULL)::int AS indexed_count,
			       COUNT(*) FILTER (WHERE c.pending AND c.embedding IS NULL)::int AS pending_count
			FROM knowledge_chunks c
			WHERE c.org_id = d.org_id AND c.doc_id = d.id) c
		ORDER BY d.created_at DESC, d.id`
	}
	return `
		SELECT d.id, d.org_id, d.filename, d.created_at, d.chunker_version, d.state, d.has_source, c.chunk_count, c.indexed_count, c.pending_count
		FROM (SELECT id, org_id, filename, created_at, chunker_version, state, content IS NOT NULL AS has_source FROM knowledge_documents
		      WHERE org_id = $1
		      ORDER BY created_at DESC, id
		      LIMIT CASE WHEN $2 > 0 THEN $2 ELSE NULL END
		      OFFSET GREATEST($3, 0)) d
		CROSS JOIN LATERAL (
			SELECT COUNT(*)::int AS chunk_count,
			       0::int AS indexed_count,
			       COUNT(*) FILTER (WHERE c.pending AND c.embedding IS NULL)::int AS pending_count
			FROM knowledge_chunks c
			WHERE c.org_id = d.org_id AND c.doc_id = d.id) c
		ORDER BY d.created_at DESC, d.id`
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
		if err := rows.Scan(&d.ID, &d.OrgID, &d.Filename, &d.CreatedAt, &d.ChunkerVersion, &d.State, &d.HasSource, &d.ChunkCount, &d.VectorIndexed, &d.PendingChunks); err != nil {
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

// inVectorIndex reports whether a vector of width observed belongs to the vector index, which holds one width:
// the configured one (PAD_EMBEDDING_DIM). It is the one rule of both paths — a chunk at another width is stored
// as JSON only (embedding_vec NULL) and a query at another width runs on the Go fallback, since cosine is
// undefined across widths. Neither path logs the mismatch, per chunk or per search: the rag layer warns once per
// org with both widths (KnowledgeService.noteEmbeddingWidth) and the knowledge listing reports them.
func inVectorIndex(configured, observed int) bool {
	return observed > 0 && observed == configured
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
		if err := lockKnowledgeOrgs(ctx, tx, chunks); err != nil {
			return err
		}
		// embedding (JSONB) is always written (portability + pgvector backfill source); embedding_vec
		// only when pgvector is active AND the width is the configured one (inVectorIndex) — mismatched
		// chunks stay NULL so the HNSW index never sees an incomparable vector. embedding_identity is
		// NULL for a chunk whose embedder named none. A chunk stored without an embedding (a pending
		// chunk still to embed, v51) has none: NULL, in no index and through no width filter. Multi-row INSERTs.
		const rowsPerStmt = 2000
		for start := 0; start < len(chunks); start += rowsPerStmt {
			end := min(start+rowsPerStmt, len(chunks))
			batch := chunks[start:end]

			var sb strings.Builder
			args := make([]any, 0, len(batch)*8)
			if b.hasPgvector {
				sb.WriteString(`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding, embedding_vec, embedding_identity, pending) VALUES `)
				for i, c := range batch {
					embJSON := knowledgeEmbeddingJSON(c.Embedding)
					var vec any
					if inVectorIndex(b.embeddingDim, len(c.Embedding)) {
						vec = FormatVector(c.Embedding)
					}
					if i > 0 {
						sb.WriteByte(',')
					}
					base := i*8 + 1
					fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d::vector,NULLIF($%d::text,''),$%d)", base, base+1, base+2, base+3, base+4, base+5, base+6, base+7)
					args = append(args, c.ID, c.DocID, c.OrgID, c.Content, embJSON, vec, c.EmbeddingIdentity, c.Pending)
				}
			} else {
				sb.WriteString(`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding, embedding_identity, pending) VALUES `)
				for i, c := range batch {
					embJSON := knowledgeEmbeddingJSON(c.Embedding)
					if i > 0 {
						sb.WriteByte(',')
					}
					base := i*7 + 1
					fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,NULLIF($%d::text,''),$%d)", base, base+1, base+2, base+3, base+4, base+5, base+6)
					args = append(args, c.ID, c.DocID, c.OrgID, c.Content, embJSON, c.EmbeddingIdentity, c.Pending)
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

// lockKnowledgeOrgs takes, in tx, the transaction-scoped advisory lock of every org the chunks belong to
// (pg_advisory_xact_lock(hashtext(org_id))), so the chunk writes of one org — an upload's insert, a re-index
// page — serialise across replicas, where rag's per-org mutex cannot reach (R2). Released with the transaction.
func lockKnowledgeOrgs(ctx context.Context, tx DBTX, chunks []interfaces.KnowledgeChunk) error {
	seen := map[string]bool{}
	var orgs []string
	for _, c := range chunks {
		if c.OrgID != "" && !seen[c.OrgID] {
			seen[c.OrgID] = true
			orgs = append(orgs, c.OrgID)
		}
	}
	sort.Strings(orgs) // one lock order for every session: a multi-org batch cannot deadlock another
	for _, org := range orgs {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, org); err != nil {
			return fmt.Errorf("lock knowledge org %s: %w", org, err)
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

// SearchKnowledge ranks the org's chunks by similarity to the query. identity restricts the candidates to the
// chunks embedded under it and the legacy rows without one (ADR 0027): a chunk of another identity is the same
// width but another model's vector, which cosine cannot tell from a miss; "" = any. The width filters stay
// separate — the identity string is not parsed for the width (a provider may retry at its native width). A pending
// chunk (v51) is never a candidate: one still to embed has no vector and no width, and one already embedded belongs
// to an upload that is not finished (NOT c.pending), whose document must not answer beside the version it replaces.
func (b *PostgresStorageBackend) SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int, identity string) ([]interfaces.KnowledgeChunk, error) {
	// pgvector path: push the similarity ordering + LIMIT into the database; only chunks at the
	// configured width are indexed (inVectorIndex), and <=> is cosine distance, so ORDER BY it ASC
	// returns the most similar.
	if b.hasPgvector && inVectorIndex(b.embeddingDim, len(queryEmbedding)) {
		return b.searchKnowledgeVector(ctx, orgID, queryEmbedding, limit, identity)
	}
	// Go-side fallback: no pgvector, or the query width is not the index's — loads a deterministic
	// capped sample and ranks in-process for portability.
	return b.searchKnowledgeGo(ctx, orgID, queryEmbedding, limit, identity)
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
func (b *PostgresStorageBackend) searchKnowledgeVector(ctx context.Context, orgID string, queryEmbedding []float32, limit int, identity string) ([]interfaces.KnowledgeChunk, error) {
	stmt, indexed := b.knowledgeVectorStatement()
	if !indexed {
		rows, err := b.query(ctx).QueryContext(ctx, stmt, orgID, FormatVector(queryEmbedding), limit, knowledgeMaxCosineDistance, b.embeddingDim, identity)
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
		rows, err := tx.QueryContext(ctx, stmt, orgID, FormatVector(queryEmbedding), limit, knowledgeMaxCosineDistance, identity)
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
// width or a non-iterating pgvector. Args: $1 org, $2 query vector, $3 limit, $4 max cosine distance, $5 width (0 = any),
// $6 embedding identity ("" = any; else that identity or none).
const knowledgeVectorSearchExactSQL = `
		SELECT nearest.id, nearest.doc_id, nearest.content, d.filename, d.chunker_version
		FROM (
			SELECT c.id, c.doc_id, c.content,
			       c.embedding_vec <=> $2::vector AS dist
			FROM knowledge_chunks c
			WHERE c.org_id = $1
			  AND c.embedding_vec IS NOT NULL
			  AND NOT c.pending
			  AND ($5 <= 0 OR vector_dims(c.embedding_vec) = $5)
			  AND ($6::text = '' OR c.embedding_identity IS NULL OR c.embedding_identity = $6::text)
			  AND c.embedding_vec <=> $2::vector <= $4
			ORDER BY dist
			LIMIT $3
		) nearest
		JOIN knowledge_documents d ON d.id = nearest.doc_id
		ORDER BY nearest.dist`

// knowledgeVectorSearchSQL is the indexed form for width dims (args $1-$4 as above, $5 the identity): the inner
// query is exactly what the HNSW index serves — the cast expression it is built on, its partial predicate verbatim,
// ORDER BY on the distance computed once, the LIMIT; the org and identity conjuncts are filters the iterative scan
// keeps walking past. The cutoff is applied OUTSIDE the inner WHERE (the k nearest filtered to the close-enough
// ones equals the nearest k of the close-enough ones, and keeping it out lets the LIMIT push into the index scan);
// the document join is outside the LIMIT too.
func knowledgeVectorSearchSQL(dims int) string {
	return fmt.Sprintf(`
		SELECT nearest.id, nearest.doc_id, nearest.content, d.filename, d.chunker_version
		FROM (
			SELECT c.id, c.doc_id, c.content,
			       c.embedding_vec::vector(%[1]d) <=> $2::vector(%[1]d) AS dist
			FROM knowledge_chunks c
			WHERE c.org_id = $1
			  AND vector_dims(c.embedding_vec) = %[1]d
			  AND NOT c.pending
			  AND ($5::text = '' OR c.embedding_identity IS NULL OR c.embedding_identity = $5::text)
			ORDER BY dist
			LIMIT $3
		) nearest
		JOIN knowledge_documents d ON d.id = nearest.doc_id
		WHERE nearest.dist <= $4
		ORDER BY nearest.dist`, dims)
}

// scanKnowledgeHits reads (id, doc_id, content, filename[, chunker_version]) rows: a statement that also selects the
// document's chunker version (the vector searches do) hands it to every hit, for rag.stripOverlapPrefix.
func scanKnowledgeHits(rows *sql.Rows) ([]interfaces.KnowledgeChunk, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var chunks []interfaces.KnowledgeChunk
	for rows.Next() {
		var c interfaces.KnowledgeChunk
		dest := []any{&c.ID, &c.DocID, &c.Content, &c.Filename}
		if len(cols) > len(dest) {
			dest = append(dest, &c.ChunkerVersion)
		}
		if err := rows.Scan(dest...); err != nil {
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
func (b *PostgresStorageBackend) searchKnowledgeGo(ctx context.Context, orgID string, queryEmbedding []float32, limit int, identity string) ([]interfaces.KnowledgeChunk, error) {
	// Cap the DB load to avoid OOM; cosine sort and truncation stay in Go. ORDER BY doc_id, id makes
	// the 500-chunk sample deterministic. The jsonb_array_length filter drops width-mismatched chunks
	// BEFORE transfer (cosine is undefined across widths; a 1536-dim embedding is ~15-30 KB of JSON).
	const maxChunks = 500
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT c.id, c.doc_id, c.content, c.embedding, d.filename, d.chunker_version
		FROM knowledge_chunks c
		JOIN knowledge_documents d ON d.id = c.doc_id
		WHERE c.org_id = $1
		  AND jsonb_array_length(c.embedding) = $2
		  AND NOT c.pending
		  AND ($4::text = '' OR c.embedding_identity IS NULL OR c.embedding_identity = $4::text)
		ORDER BY c.doc_id, c.id
		LIMIT $3`, orgID, len(queryEmbedding), maxChunks, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []interfaces.KnowledgeChunk
	for rows.Next() {
		var c interfaces.KnowledgeChunk
		var embJSON []byte
		if err := rows.Scan(&c.ID, &c.DocID, &c.Content, &embJSON, &c.Filename, &c.ChunkerVersion); err != nil {
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
	// invisible — surface it, with why the fallback ran (no pgvector, or a query width that is not the
	// index's), so ops knows it is lossy here (install pgvector, or make PAD_EMBEDDING_DIM the model's
	// width and re-index).
	if len(chunks) >= maxChunks {
		logger.Warn("knowledge search fallback capped — chunks beyond the sample are unsearchable",
			"org_id", orgID, "sampled", len(chunks), "query_dim", len(queryEmbedding), "configured_dim", b.embeddingDim, "pgvector", b.hasPgvector)
		recordRAGFallbackCapped()
	}

	return rankKnowledgeChunks(chunks, queryEmbedding, limit)
}

// ListKnowledgeChunkContents returns one page of chunks (no embeddings) strictly after the (doc_id, id) cursor,
// for re-indexing: the embedding provider changed, but the chunked TEXT is still the source of truth. With
// notIdentity the page skips the chunks already embedded under it (legacy rows without one are walked), so a
// re-index converts only what is not yet at the current model and a restarted run never re-buys a page. A
// pending chunk (v51) is its upload's to embed and is never on a page.
func (b *PostgresStorageBackend) ListKnowledgeChunkContents(ctx context.Context, orgID string, after interfaces.KnowledgeChunkCursor, limit int, notIdentity string) ([]interfaces.KnowledgeChunk, error) {
	return b.listKnowledgeChunkContents(ctx, orgID, after, limit, notIdentity, false)
}

// ListUnindexedKnowledgeChunkContents is the page of the chunks outside the vector index (embedding_vec NULL):
// the rows a PAD_EMBEDDING_DIM change left as JSON only, which carry the caller's own identity and so are not
// what notIdentity can single out. Without pgvector there is no column to test and every chunk is returned.
func (b *PostgresStorageBackend) ListUnindexedKnowledgeChunkContents(ctx context.Context, orgID string, after interfaces.KnowledgeChunkCursor, limit int) ([]interfaces.KnowledgeChunk, error) {
	return b.listKnowledgeChunkContents(ctx, orgID, after, limit, "", b.hasPgvector)
}

// listKnowledgeChunkContentsSQL is the re-index page statement; unindexedOnly adds the vector-column conjunct,
// which exists only with pgvector (the callers decide).
func listKnowledgeChunkContentsSQL(unindexedOnly bool) string {
	unindexed := ""
	if unindexedOnly {
		unindexed = "\n\t\t  AND c.embedding_vec IS NULL"
	}
	return `
		SELECT c.id, c.doc_id, c.content
		FROM knowledge_chunks c
		WHERE c.org_id = $1
		  AND (c.doc_id, c.id) > ($2::text, $3::text)
		  AND NOT c.pending
		  AND ($5::text = '' OR c.embedding_identity IS DISTINCT FROM $5::text)` + unindexed + `
		ORDER BY c.doc_id, c.id
		LIMIT $4`
}

func (b *PostgresStorageBackend) listKnowledgeChunkContents(ctx context.Context, orgID string, after interfaces.KnowledgeChunkCursor, limit int, notIdentity string, unindexedOnly bool) ([]interfaces.KnowledgeChunk, error) {
	const maxPage = 2000
	if limit <= 0 || limit > maxPage {
		limit = maxPage
	}
	rows, err := b.query(ctx).QueryContext(ctx, listKnowledgeChunkContentsSQL(unindexedOnly), orgID, after.DocID, after.ID, limit, notIdentity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []interfaces.KnowledgeChunk
	for rows.Next() {
		c := interfaces.KnowledgeChunk{OrgID: orgID}
		if err := rows.Scan(&c.ID, &c.DocID, &c.Content); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// sampleKnowledgeChunkEmbeddingsSQL is the SampleKnowledgeChunkEmbeddings statement (exposed for the plan tests).
// Two literal statements (not a spliced ORDER BY): embedding_vec exists only with pgvector.
//
// With pgvector a chunk in the vector index has an embedding (embedding_vec is written from the JSON array, never
// from an empty one), and IS NOT NULL reads the row's null bitmap only: the JSON embedding (15-30 KB, TOASTed) is
// detoasted for the guard only where embedding_vec is NULL, the OR's second arm (Postgres evaluates an OR's arms in
// order and stops at the first true one; plans_knowledge_sample_test.go measures the blocks read). The sample is
// picked on ids, and only the n picked rows are read back for their content and embedding.
func sampleKnowledgeChunkEmbeddingsSQL(pgvector bool) string {
	if pgvector {
		return `
		SELECT c.id, c.doc_id, c.content, c.embedding FROM (
			SELECT id, doc_id, unindexed FROM (
				SELECT DISTINCT ON (c.doc_id) c.id, c.doc_id, (c.embedding_vec IS NULL) AS unindexed
				FROM knowledge_chunks c
				WHERE c.org_id = $1
				  AND NOT c.pending
				  AND (c.embedding_vec IS NOT NULL
				       OR CASE WHEN jsonb_typeof(c.embedding) = 'array'
				               THEN jsonb_array_length(c.embedding) ELSE 0 END > 0)
				ORDER BY c.doc_id, (c.embedding_vec IS NULL), c.id
			) d
			ORDER BY unindexed, doc_id
			LIMIT $2
		) s
		JOIN knowledge_chunks c ON c.id = s.id
		ORDER BY s.unindexed, s.doc_id`
	}
	return `
		SELECT id, doc_id, content, embedding FROM (
			SELECT DISTINCT ON (c.doc_id) c.id, c.doc_id, c.content, c.embedding
			FROM knowledge_chunks c
			WHERE c.org_id = $1
			  AND NOT c.pending
			  AND CASE WHEN jsonb_typeof(c.embedding) = 'array'
			           THEN jsonb_array_length(c.embedding) ELSE 0 END > 0
			ORDER BY c.doc_id, c.id
		) s
		ORDER BY doc_id
		LIMIT $2`
}

// SampleKnowledgeChunkEmbeddings returns up to n of the org's chunks with their stored embeddings — ONE per
// document — for the RAG embedding-model mismatch guard (DISTINCT ON spreads the sample over the corpus; "the
// n lowest ids" would be a fixed subset a partially converted corpus can hide from). With pgvector, each
// document's indexed chunk is picked first; the CASE-form jsonb guard keeps rows without an embedding array
// out (jsonb_array_length would error on a scalar, and Postgres need not short-circuit an AND). A pending chunk (v51)
// is not in the corpus yet, embedded or not: never sampled, here or by the probes below.
func (b *PostgresStorageBackend) SampleKnowledgeChunkEmbeddings(ctx context.Context, orgID string, n int) ([]interfaces.KnowledgeChunk, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx, sampleKnowledgeChunkEmbeddingsSQL(b.hasPgvector), orgID, n)
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

// unindexedKnowledgeChunkWidthSQL is the UnindexedKnowledgeChunkWidth statement (pgvector only): the first row of
// the org outside the vector index that has an embedding array, and that array's length. embedding_vec IS NULL
// is the null bitmap; the CASE detoasts the JSON of the rows outside the index alone, and the LIMIT stops at the
// first — unlike the guard's sample, which prefers indexed rows and reads one per document of the org.
const unindexedKnowledgeChunkWidthSQL = `
		SELECT CASE WHEN jsonb_typeof(c.embedding) = 'array' THEN jsonb_array_length(c.embedding) ELSE 0 END AS width
		FROM knowledge_chunks c
		WHERE c.org_id = $1
		  AND c.embedding_vec IS NULL
		  AND NOT c.pending
		  AND CASE WHEN jsonb_typeof(c.embedding) = 'array' THEN jsonb_array_length(c.embedding) ELSE 0 END > 0
		LIMIT 1`

// UnindexedKnowledgeChunkWidth is the stored width of one of the org's chunks outside the vector index, for the
// knowledge listing's width report; 0 when none is (or, without pgvector, when there is no index to be outside of).
func (b *PostgresStorageBackend) UnindexedKnowledgeChunkWidth(ctx context.Context, orgID string) (int, error) {
	if !b.hasPgvector {
		return 0, nil
	}
	var width int
	err := b.query(ctx).QueryRowContext(ctx, unindexedKnowledgeChunkWidthSQL, orgID).Scan(&width)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return width, err
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
		  AND NOT c.pending
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
		if err := lockKnowledgeOrgs(ctx, tx, chunks); err != nil {
			return err
		}
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
// The identity is replaced along with the vector: NULL when the embedder named none. A pending chunk (v51) stays
// pending — out of the corpus — until its document is finished (FinishKnowledgeDocument).
func buildKnowledgeEmbeddingUpdate(batch []interfaces.KnowledgeChunk, hasPgvector bool, embeddingDim int) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, len(batch)*4)
	if hasPgvector {
		sb.WriteString(`UPDATE knowledge_chunks AS c SET embedding = v.emb, embedding_vec = v.vec, embedding_identity = NULLIF(v.ident, '')
			FROM (VALUES `)
		for i, c := range batch {
			embJSON, _ := json.Marshal(c.Embedding)
			var vec any
			if inVectorIndex(embeddingDim, len(c.Embedding)) {
				vec = FormatVector(c.Embedding)
			}
			if i > 0 {
				sb.WriteByte(',')
			}
			base := i*4 + 1
			fmt.Fprintf(&sb, "($%d::text, $%d::jsonb, $%d::vector, $%d::text)", base, base+1, base+2, base+3)
			args = append(args, c.ID, embJSON, vec, c.EmbeddingIdentity)
		}
		sb.WriteString(`) AS v(id, emb, vec, ident) WHERE c.id = v.id`)
		return sb.String(), args
	}
	sb.WriteString(`UPDATE knowledge_chunks AS c SET embedding = v.emb, embedding_identity = NULLIF(v.ident, '')
		FROM (VALUES `)
	for i, c := range batch {
		embJSON, _ := json.Marshal(c.Embedding)
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*3 + 1
		fmt.Fprintf(&sb, "($%d::text, $%d::jsonb, $%d::text)", base, base+1, base+2)
		args = append(args, c.ID, embJSON, c.EmbeddingIdentity)
	}
	sb.WriteString(`) AS v(id, emb, ident) WHERE c.id = v.id`)
	return sb.String(), args
}

// CountKnowledgeChunks reports the org's total chunks and how many sit in the pgvector index at the
// configured dimension — total>0 with 0 indexed (pgvector active) means an embedding switch stranded the corpus.
// A pending chunk (v51) is not in the corpus until its upload embeds it, here and in the census by identity.
func (b *PostgresStorageBackend) CountKnowledgeChunks(ctx context.Context, orgID string) (int, int, error) {
	var total, indexed int
	query := `
		SELECT COUNT(c.id), 0
		FROM knowledge_chunks c
		WHERE c.org_id = $1 AND NOT c.pending`
	if b.hasPgvector {
		query = `
		SELECT COUNT(c.id), COUNT(c.id) FILTER (WHERE c.embedding_vec IS NOT NULL)
		FROM knowledge_chunks c
		WHERE c.org_id = $1 AND NOT c.pending`
	}
	err := b.query(ctx).QueryRowContext(ctx, query, orgID).Scan(&total, &indexed)
	return total, indexed, err
}

// CountKnowledgeChunksByIdentity is CountKnowledgeChunks with the census by embedding identity (ADR 0027): one
// aggregate pass over the org's chunks; with "" nothing is current or stranded, so every row counts as legacy.
func (b *PostgresStorageBackend) CountKnowledgeChunksByIdentity(ctx context.Context, orgID, identity string) (interfaces.KnowledgeChunkCounts, error) {
	var c interfaces.KnowledgeChunkCounts
	query := `
		SELECT COUNT(c.id), 0,
		       COUNT(c.id) FILTER (WHERE $2::text <> '' AND c.embedding_identity = $2::text),
		       COUNT(c.id) FILTER (WHERE $2::text <> '' AND c.embedding_identity IS NOT NULL AND c.embedding_identity <> $2::text)
		FROM knowledge_chunks c
		WHERE c.org_id = $1 AND NOT c.pending`
	if b.hasPgvector {
		query = `
		SELECT COUNT(c.id), COUNT(c.id) FILTER (WHERE c.embedding_vec IS NOT NULL),
		       COUNT(c.id) FILTER (WHERE $2::text <> '' AND c.embedding_identity = $2::text),
		       COUNT(c.id) FILTER (WHERE $2::text <> '' AND c.embedding_identity IS NOT NULL AND c.embedding_identity <> $2::text)
		FROM knowledge_chunks c
		WHERE c.org_id = $1 AND NOT c.pending`
	}
	err := b.query(ctx).QueryRowContext(ctx, query, orgID, identity).Scan(&c.Total, &c.VectorIndexed, &c.Current, &c.Stranded)
	return c, err
}

// CountStrandedKnowledgeChunksByDocument counts the org's chunks of another identity per document, for the
// admin list; the walk is the org's chunk index (org_id, doc_id, id). "" strands nothing.
func (b *PostgresStorageBackend) CountStrandedKnowledgeChunksByDocument(ctx context.Context, orgID, identity string) (map[string]int, error) {
	out := map[string]int{}
	if identity == "" {
		return out, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT c.doc_id, COUNT(c.id)::int
		FROM knowledge_chunks c
		WHERE c.org_id = $1
		  AND c.embedding_identity IS NOT NULL AND c.embedding_identity <> $2::text
		GROUP BY c.doc_id`, orgID, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var docID string
		var n int
		if err := rows.Scan(&docID, &n); err != nil {
			return nil, err
		}
		out[docID] = n
	}
	return out, rows.Err()
}

// StampKnowledgeChunkIdentity rewrites the identity of the org's chunks at from ("" = the legacy NULL rows) to
// to ("" = NULL): the lenient legacy match ends for an org once an upload or re-index proved its model. Its own
// short RLS transaction as userID, like the other chunk writers: the org predicate is not the only tenant guard.
func (b *PostgresStorageBackend) StampKnowledgeChunkIdentity(ctx context.Context, userID, orgID, from, to string) (n int, err error) {
	err = b.withTxAs(ctx, userID, func(_ context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE knowledge_chunks SET embedding_identity = NULLIF($3::text, '')
			WHERE org_id = $1 AND embedding_identity IS NOT DISTINCT FROM NULLIF($2::text, '')`, orgID, from, to)
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		n = int(rows)
		return err
	})
	return n, err
}

// rankKnowledgeChunks scores chunks against queryEmbedding by cosine similarity and returns the top `limit`,
// highest first. Width-mismatched chunks are skipped rather than scored 0; if nothing is comparable, it fails
// loudly so the caller re-indexes instead of receiving silently-irrelevant results.
func rankKnowledgeChunks(chunks []interfaces.KnowledgeChunk, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
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
	// A chunk at another width is skipped without a log line: the rag layer warns once per org (inVectorIndex).
	if len(scoredChunks) == 0 && mismatched > 0 {
		return nil, fmt.Errorf("knowledge search embedding dimension mismatch: query has %d dims but all %d stored chunks differ — re-index the knowledge base after changing the embedding provider", qDim, mismatched)
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
