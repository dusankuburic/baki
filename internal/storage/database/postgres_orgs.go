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
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// ---- Organisation operations ----

func (b *PostgresStorageBackend) SaveOrg(ctx context.Context, org *interfaces.Organisation) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if org.CreatedAt.IsZero() {
		org.CreatedAt = now
	}
	org.UpdatedAt = now

	_, err = tx.ExecContext(ctx, `
		INSERT INTO organisations (id, name, owner_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			owner_id = EXCLUDED.owner_id,
			updated_at = EXCLUDED.updated_at`,
		org.ID, org.Name, org.OwnerID, org.CreatedAt, org.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert org: %w", err)
	}

	if err := syncOrgMembers(ctx, tx, org, now); err != nil {
		return err
	}

	return tx.Commit()
}

// syncOrgMembers reconciles org_members to exactly org.Members without the
// churn of delete-all-then-reinsert: present members are upserted (their
// joined_at is preserved on conflict) and only members no longer in the set are
// deleted. now is the fallback joined_at for members without one. Shared by
// SaveOrg and saveOrgInTx.
func syncOrgMembers(ctx context.Context, tx *sql.Tx, org *interfaces.Organisation, now time.Time) error {
	userIDs := make([]string, 0, len(org.Members))
	for _, m := range org.Members {
		joined := m.JoinedAt
		if joined.IsZero() {
			joined = now
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO org_members (org_id, user_id, role, joined_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
			org.ID, m.UserID, string(m.Role), joined); err != nil {
			return fmt.Errorf("upsert member: %w", err)
		}
		userIDs = append(userIDs, m.UserID)
	}
	// Delete members no longer present. An empty userIDs slice makes
	// `user_id = ANY('{}')` always false, so NOT(...) deletes every row —
	// matching the old delete-all behavior when the set is emptied.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM org_members WHERE org_id = $1 AND NOT (user_id = ANY($2))`,
		org.ID, userIDs); err != nil {
		return fmt.Errorf("prune removed members: %w", err)
	}
	return nil
}

func (b *PostgresStorageBackend) LoadOrg(ctx context.Context, id string) (*interfaces.Organisation, error) {
	row := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, name, owner_id, created_at, updated_at FROM organisations WHERE id = $1`, id)

	var org interfaces.Organisation
	if err := row.Scan(&org.ID, &org.Name, &org.OwnerID, &org.CreatedAt, &org.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("scan org: %w", err)
	}

	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT user_id, role, joined_at FROM org_members WHERE org_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("query members: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var m interfaces.OrgMember
		var roleStr string
		if err := rows.Scan(&m.UserID, &roleStr, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		m.Role = auth.Role(roleStr)
		org.Members = append(org.Members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate members: %w", err)
	}

	return &org, nil
}

func (b *PostgresStorageBackend) ListOrgsForUser(ctx context.Context, userID string) ([]*interfaces.Organisation, error) {
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT o.id, o.name, o.owner_id, o.created_at, o.updated_at
		FROM organisations o
		JOIN org_members m ON o.id = m.org_id
		WHERE m.user_id = $1
		ORDER BY o.updated_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("query orgs: %w", err)
	}
	defer rows.Close()

	var orgs []*interfaces.Organisation
	for rows.Next() {
		var org interfaces.Organisation
		if err := rows.Scan(&org.ID, &org.Name, &org.OwnerID, &org.CreatedAt, &org.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan org: %w", err)
		}
		orgs = append(orgs, &org)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate orgs: %w", err)
	}

	// We don't load full member lists for the user's org list to keep it fast.
	// If needed, the caller can LoadOrg(id) for a specific one.
	return orgs, nil
}

func (b *PostgresStorageBackend) DeleteOrg(ctx context.Context, id string) error {
	_, err := b.query(ctx).ExecContext(ctx, `DELETE FROM organisations WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete org: %w", err)
	}
	return nil
}

func (b *PostgresStorageBackend) MutateOrg(ctx context.Context, id string, fn func(*interfaces.Organisation) error) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mutate org begin tx: %w", err)
	}
	defer tx.Rollback()

	org, err := b.loadOrgForUpdate(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := fn(org); err != nil {
		return err
	}
	if err := b.saveOrgInTx(ctx, tx, org); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *PostgresStorageBackend) loadOrgForUpdate(ctx context.Context, tx *sql.Tx, id string) (*interfaces.Organisation, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, name, owner_id, created_at, updated_at FROM organisations WHERE id = $1 FOR UPDATE`, id)
	var org interfaces.Organisation
	if err := row.Scan(&org.ID, &org.Name, &org.OwnerID, &org.CreatedAt, &org.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("organisation not found: %s", id)
		}
		return nil, fmt.Errorf("load org for update: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id, role, joined_at FROM org_members WHERE org_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("load org members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m interfaces.OrgMember
		var roleStr string
		if err := rows.Scan(&m.UserID, &roleStr, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan org member: %w", err)
		}
		m.Role = auth.Role(roleStr)
		org.Members = append(org.Members, m)
	}
	return &org, rows.Err()
}

func (b *PostgresStorageBackend) saveOrgInTx(ctx context.Context, tx *sql.Tx, org *interfaces.Organisation) error {
	_, err := tx.ExecContext(ctx, `UPDATE organisations SET name = $1, owner_id = $2, updated_at = $3 WHERE id = $4`,
		org.Name, org.OwnerID, org.UpdatedAt, org.ID)
	if err != nil {
		return fmt.Errorf("update org in tx: %w", err)
	}
	return syncOrgMembers(ctx, tx, org, time.Now().UTC())
}

// ---- Knowledge base operations ----

func (b *PostgresStorageBackend) SaveKnowledgeDocument(ctx context.Context, doc *interfaces.KnowledgeDocument) error {
	_, err := b.query(ctx).ExecContext(ctx, `INSERT INTO knowledge_documents (id, org_id, filename) VALUES ($1, $2, $3)`,
		doc.ID, doc.OrgID, doc.Filename)
	return err
}

func (b *PostgresStorageBackend) DeleteKnowledgeDocument(ctx context.Context, orgID, id string) error {
	_, err := b.query(ctx).ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1 AND org_id = $2`, id, orgID)
	return err
}

func (b *PostgresStorageBackend) ListKnowledgeDocuments(ctx context.Context, orgID string) ([]*interfaces.KnowledgeDocument, error) {
	rows, err := b.query(ctx).QueryContext(ctx, `SELECT id, org_id, filename, created_at FROM knowledge_documents WHERE org_id = $1`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []*interfaces.KnowledgeDocument
	for rows.Next() {
		var d interfaces.KnowledgeDocument
		if err := rows.Scan(&d.ID, &d.OrgID, &d.Filename, &d.CreatedAt); err != nil {
			return nil, err
		}
		docs = append(docs, &d)
	}
	return docs, rows.Err()
}

// SaveKnowledgeChunks inserts chunk rows. The Postgres knowledge_chunks table
// has RLS policies; to honor them the inserts MUST run in a transaction with
// app.current_user_id set (BeginRLS) — otherwise app_rls_active() returns
// false and the WITH CHECK policy short-circuits to "allow", letting any
// authenticated caller write chunks for arbitrary org_id values.
//
// If the RLS middleware already opened a tx on ctx we run inside it; otherwise
// we open and commit our own RLS-scoped tx.
func (b *PostgresStorageBackend) SaveKnowledgeChunks(ctx context.Context, userID string, chunks []interfaces.KnowledgeChunk) error {
	if len(chunks) == 0 {
		return nil
	}

	runInserts := func(tx DBTX) error {
		// embedding (JSONB) is always written for portability + as the pgvector
		// backfill source. embedding_vec is written only when pgvector is active
		// AND the chunk's embedding dimension matches the configured contract —
		// mismatched-dimension chunks stay NULL in the vector column so the
		// HNSW index search never sees a vector it can't compare to a query.
		if b.hasPgvector {
			const stmt = `INSERT INTO knowledge_chunks (id, doc_id, content, embedding, embedding_vec) VALUES ($1, $2, $3, $4, $5::vector)`
			for _, c := range chunks {
				embJSON, _ := json.Marshal(c.Embedding)
				var vec any
				if len(c.Embedding) == b.embeddingDim {
					vec = FormatVector(c.Embedding)
				} else if len(c.Embedding) > 0 {
					logger.Warn("knowledge chunk embedding dimension differs from configured; excluded from vector index",
						"chunk_id", c.ID, "got_dim", len(c.Embedding), "configured_dim", b.embeddingDim)
				}
				if _, err := tx.ExecContext(ctx, stmt, c.ID, c.DocID, c.Content, embJSON, vec); err != nil {
					return err
				}
			}
			return nil
		}
		const stmt = `INSERT INTO knowledge_chunks (id, doc_id, content, embedding) VALUES ($1, $2, $3, $4)`
		for _, c := range chunks {
			embJSON, _ := json.Marshal(c.Embedding)
			if _, err := tx.ExecContext(ctx, stmt, c.ID, c.DocID, c.Content, embJSON); err != nil {
				return err
			}
		}
		return nil
	}

	// Reuse the middleware RLS tx if present.
	if existingTx, ok := ctx.Value(rlsTxKey).(*sql.Tx); ok && existingTx != nil {
		return runInserts(existingTx)
	}

	// No existing tx — open and commit our own RLS-scoped tx.
	tx, err := b.BeginRLS(ctx, userID)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := runInserts(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save knowledge chunks: commit: %w", err)
	}
	committed = true
	return nil
}

func (b *PostgresStorageBackend) SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	// pgvector path: push the similarity ordering + LIMIT into the database so
	// we don't load hundreds of embeddings into Go. Only same-dimension chunks
	// are indexed (mismatched dims are NULL in embedding_vec and excluded by the
	// WHERE). <=> is cosine distance (0 = identical, 2 = opposite); ORDER BY it
	// ASC returns the most similar chunks.
	if b.hasPgvector && len(queryEmbedding) == b.embeddingDim {
		return b.searchKnowledgeVector(ctx, orgID, queryEmbedding, limit)
	}
	// Go-side fallback: pgvector isn't installed, or the query dimension doesn't
	// match the configured index dimension (e.g. the embedding provider changed
	// and the base hasn't been re-indexed). Loads a deterministic capped sample
	// and ranks in-process for portability.
	return b.searchKnowledgeGo(ctx, orgID, queryEmbedding, limit)
}

// searchKnowledgeVector runs the server-side similarity search. A cosine
// distance threshold of 1.0 (max distance = 2.0) excludes irrelevant chunks
// — a query with no genuinely-relevant docs returns empty instead of noise.
func (b *PostgresStorageBackend) searchKnowledgeVector(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT c.id, c.doc_id, c.content, c.embedding
		FROM knowledge_chunks c
		JOIN knowledge_documents d ON c.doc_id = d.id
		WHERE d.org_id = $1
		  AND c.embedding_vec IS NOT NULL
		  AND c.embedding_vec <=> $2::vector < 1.0
		ORDER BY c.embedding_vec <=> $2::vector
		LIMIT $3`, orgID, FormatVector(queryEmbedding), limit)
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
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate knowledge chunks: %w", err)
	}
	return chunks, nil
}

// searchKnowledgeGo is the portability fallback: load a deterministic capped
// sample and rank in-process via cosine similarity.
func (b *PostgresStorageBackend) searchKnowledgeGo(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	// Cap the number of chunks loaded from DB to avoid OOM on large
	// knowledge bases. The cosine similarity sort and final truncation
	// still happens in Go for portability (no pgvector dependency).
	// ORDER BY doc_id, id makes the 500-chunk sample deterministic (same
	// chunks on repeated calls) — without it, Postgres returns an arbitrary
	// set of rows depending on physical layout / vacuum state.
	const maxChunks = 500
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT c.id, c.doc_id, c.content, c.embedding
		FROM knowledge_chunks c
		JOIN knowledge_documents d ON c.doc_id = d.id
		WHERE d.org_id = $1
		ORDER BY c.doc_id, c.id
		LIMIT $2`, orgID, maxChunks)
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
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate knowledge chunks: %w", err)
	}

	return rankKnowledgeChunks(orgID, chunks, queryEmbedding, limit)
}

// rankKnowledgeChunks scores chunks against queryEmbedding by cosine similarity
// and returns the top `limit`, highest first.
//
// Chunks whose embedding width differs from the query's cannot be compared
// (cosine similarity is only defined for equal-length vectors) — this happens
// when the knowledge base was indexed with a different embedding provider/model
// than the one answering the query. Such chunks are skipped rather than scored
// 0 (which would rank them as arbitrary noise); if nothing is comparable, it
// fails loudly so the caller re-indexes instead of receiving silently-irrelevant
// results.
func rankKnowledgeChunks(orgID string, chunks []interfaces.KnowledgeChunk, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	// Score each chunk once, then sort by score. Computing similarity inside the
	// comparator would recompute each chunk's score O(n log n) times.
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

	// Filter by minimum similarity (0.5 cosine = 1.0 cosine distance, matching
	// the pgvector path's threshold). Chunks below this are noise.
	cutoff := 0
	for cutoff < len(scoredChunks) && scoredChunks[cutoff].sim >= 0.5 {
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

// FormatVector renders a float32 embedding as the pgvector text literal
// (`'[0.1,0.2,…]'`) that the vector type's input function accepts. Used for
// both INSERT (embedding_vec) and the <=> query operand. A nil/empty slice
// yields '[]' which pgvector rejects — callers must guard non-empty inputs.
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
