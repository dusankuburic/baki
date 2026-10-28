package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// ---- Two-phase uploads, source texts and the per-document identity stamp (v51) ----

// knowledgeEmbeddingJSON is the stored value of a chunk's embedding: its JSON array, or NULL for a chunk stored
// without one (a pending chunk still to embed, v51) — never JSON null, on which jsonb_array_length (the in-process
// search's width filter) fails.
func knowledgeEmbeddingJSON(embedding []float32) any {
	if len(embedding) == 0 {
		return nil
	}
	out, _ := json.Marshal(embedding)
	return out
}

// getKnowledgeDocumentSQL reads one document of an org with its source text and its chunk counts (the document's
// chunks by the org's (org_id, doc_id, id) index).
const getKnowledgeDocumentSQL = `
		SELECT d.id, d.org_id, d.filename, d.created_at, d.chunker_version, d.state,
		       COALESCE(d.content, ''), d.content IS NOT NULL,
		       (SELECT COUNT(*)::int FROM knowledge_chunks c WHERE c.org_id = d.org_id AND c.doc_id = d.id),
		       (SELECT COUNT(*)::int FROM knowledge_chunks c WHERE c.org_id = d.org_id AND c.doc_id = d.id AND c.pending AND c.embedding IS NULL)
		FROM knowledge_documents d
		WHERE d.org_id = $1 AND d.id = $2`

// GetKnowledgeDocument reads one of the org's documents with its source text; ErrNotFound when there is none.
func (b *PostgresStorageBackend) GetKnowledgeDocument(ctx context.Context, orgID, id string) (*interfaces.KnowledgeDocument, error) {
	var d interfaces.KnowledgeDocument
	err := b.query(ctx).QueryRowContext(ctx, getKnowledgeDocumentSQL, orgID, id).Scan(&d.ID, &d.OrgID, &d.Filename, &d.CreatedAt,
		&d.ChunkerVersion, &d.State, &d.Content, &d.HasSource, &d.ChunkCount, &d.PendingChunks)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListPendingKnowledgeChunks is one page of a document's pending chunks still to embed (embedding NULL: the null
// bitmap, no detoasting) in id order — the walk of a two-phase upload, on the org's (org_id, doc_id, id) index.
func (b *PostgresStorageBackend) ListPendingKnowledgeChunks(ctx context.Context, orgID, docID, afterID string, limit int) ([]interfaces.KnowledgeChunk, error) {
	const maxPage = 2000
	if limit <= 0 || limit > maxPage {
		limit = maxPage
	}
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT c.id, c.doc_id, c.content
		FROM knowledge_chunks c
		WHERE c.org_id = $1 AND c.doc_id = $2 AND c.id > $3 AND c.pending AND c.embedding IS NULL
		ORDER BY c.id
		LIMIT $4`, orgID, docID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []interfaces.KnowledgeChunk
	for rows.Next() {
		c := interfaces.KnowledgeChunk{OrgID: orgID, Pending: true}
		if err := rows.Scan(&c.ID, &c.DocID, &c.Content); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// FinishKnowledgeDocument turns a document none of whose chunks lacks its embedding ready, puts its chunks into the
// corpus (pending cleared) and retires the older ready versions of its filename, in one short RLS transaction as
// userID: the corpus swaps versions at once — the old one serves until the new one is complete, never beside it. The org's chunk-write lock is taken first, as every chunk
// writer takes it, and the document row is locked, so a page committing beside the finish is counted or waits.
func (b *PostgresStorageBackend) FinishKnowledgeDocument(ctx context.Context, userID, orgID, docID string) (pending int, err error) {
	err = b.withTxAs(ctx, userID, func(txCtx context.Context, tx *sql.Tx) error {
		if err := lockKnowledgeOrgs(txCtx, tx, []interfaces.KnowledgeChunk{{OrgID: orgID}}); err != nil {
			return err
		}
		var state, filename string
		err := tx.QueryRowContext(txCtx, `SELECT state, filename FROM knowledge_documents WHERE id = $1 AND org_id = $2 FOR UPDATE`, docID, orgID).Scan(&state, &filename)
		if errors.Is(err, sql.ErrNoRows) {
			return interfaces.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(txCtx, `SELECT COUNT(*)::int FROM knowledge_chunks WHERE org_id = $1 AND doc_id = $2 AND pending AND embedding IS NULL`, orgID, docID).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 || state == interfaces.KnowledgeDocumentReady {
			return nil
		}
		if _, err := tx.ExecContext(txCtx, `UPDATE knowledge_chunks SET pending = false WHERE org_id = $1 AND doc_id = $2 AND pending`, orgID, docID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(txCtx, `UPDATE knowledge_documents SET state = 'ready' WHERE id = $1 AND org_id = $2`, docID, orgID); err != nil {
			return err
		}
		// The versions this one supersedes: ready documents of its name (a pending one is another upload's, which
		// superseded or resumed this one under the org's lease and is not this finish's to retire).
		_, err = tx.ExecContext(txCtx, `DELETE FROM knowledge_documents WHERE org_id = $1 AND filename = $2 AND id <> $3 AND state = 'ready'`, orgID, filename, docID)
		return err
	})
	return pending, err
}

// ListKnowledgeDocumentsBelowChunkerVersion pages the org's ready documents cut by an older chunker, in id order.
func (b *PostgresStorageBackend) ListKnowledgeDocumentsBelowChunkerVersion(ctx context.Context, orgID string, version int, afterID string, limit int) ([]*interfaces.KnowledgeDocument, error) {
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT id, org_id, filename, created_at, chunker_version, state, content IS NOT NULL
		FROM knowledge_documents
		WHERE org_id = $1 AND state = 'ready' AND chunker_version < $2 AND id > $3
		ORDER BY id
		LIMIT CASE WHEN $4 > 0 THEN $4 ELSE NULL END`, orgID, version, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []*interfaces.KnowledgeDocument
	for rows.Next() {
		var d interfaces.KnowledgeDocument
		if err := rows.Scan(&d.ID, &d.OrgID, &d.Filename, &d.CreatedAt, &d.ChunkerVersion, &d.State, &d.HasSource); err != nil {
			return nil, err
		}
		docs = append(docs, &d)
	}
	return docs, rows.Err()
}

// sampleLegacyKnowledgeChunkEmbeddingsSQL picks the first $2 documents (doc_id order) holding legacy chunks, then each
// one's first and last legacy chunk by id from the org's (org_id, doc_id, id) index, and reads only those rows' text
// and embedding. A re-index that stopped inside a document converted a prefix of it in id order, so the two ends sit
// on either side of the cut.
const sampleLegacyKnowledgeChunkEmbeddingsSQL = `
		WITH docs AS (
			SELECT DISTINCT c.doc_id
			FROM knowledge_chunks c
			WHERE c.org_id = $1 AND c.embedding_identity IS NULL AND NOT c.pending
			ORDER BY c.doc_id
			LIMIT $2
		), ends AS (
			SELECT (SELECT c.id FROM knowledge_chunks c
			        WHERE c.org_id = $1 AND c.doc_id = docs.doc_id AND c.embedding_identity IS NULL AND NOT c.pending
			        ORDER BY c.id LIMIT 1) AS first_id,
			       (SELECT c.id FROM knowledge_chunks c
			        WHERE c.org_id = $1 AND c.doc_id = docs.doc_id AND c.embedding_identity IS NULL AND NOT c.pending
			        ORDER BY c.id DESC LIMIT 1) AS last_id
			FROM docs
		)
		SELECT c.id, c.doc_id, c.content, c.embedding
		FROM knowledge_chunks c
		WHERE c.org_id = $1 AND c.id IN (SELECT first_id FROM ends UNION SELECT last_id FROM ends)
		ORDER BY c.doc_id, c.id`

// SampleLegacyKnowledgeChunkEmbeddings returns the two ends of the legacy chunks of up to docs documents, with their
// stored embeddings, for the per-document identity proof (rag.checkCorpusModel).
func (b *PostgresStorageBackend) SampleLegacyKnowledgeChunkEmbeddings(ctx context.Context, orgID string, docs int) ([]interfaces.KnowledgeChunk, error) {
	if docs <= 0 {
		return nil, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx, sampleLegacyKnowledgeChunkEmbeddingsSQL, orgID, docs)
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

// StampKnowledgeDocumentIdentity stamps the legacy chunks of the proven documents alone, in a short RLS transaction as
// userID like the other chunk writers. A pending chunk carries no identity yet and is not legacy: never stamped.
func (b *PostgresStorageBackend) StampKnowledgeDocumentIdentity(ctx context.Context, userID, orgID string, docIDs []string, to string) (n int, err error) {
	if len(docIDs) == 0 {
		return 0, nil
	}
	err = b.withTxAs(ctx, userID, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(txCtx, `
			UPDATE knowledge_chunks SET embedding_identity = NULLIF($3::text, '')
			WHERE org_id = $1 AND doc_id = ANY($2) AND embedding_identity IS NULL AND NOT pending`, orgID, docIDs, to)
		if err != nil {
			return fmt.Errorf("stamp knowledge document identity: %w", err)
		}
		rows, err := res.RowsAffected()
		n = int(rows)
		return err
	})
	return n, err
}
