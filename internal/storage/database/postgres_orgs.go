package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
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

	// Simple member sync: delete all and re-insert
	_, err = tx.ExecContext(ctx, `DELETE FROM org_members WHERE org_id = $1`, org.ID)
	if err != nil {
		return fmt.Errorf("clear members: %w", err)
	}

	for _, m := range org.Members {
		if m.JoinedAt.IsZero() {
			m.JoinedAt = now
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO org_members (org_id, user_id, role, joined_at)
			VALUES ($1, $2, $3, $4)`,
			org.ID, m.UserID, string(m.Role), m.JoinedAt)
		if err != nil {
			return fmt.Errorf("insert member: %w", err)
		}
	}

	return tx.Commit()
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
	_, err = tx.ExecContext(ctx, `DELETE FROM org_members WHERE org_id = $1`, org.ID)
	if err != nil {
		return fmt.Errorf("clear org members in tx: %w", err)
	}
	for _, m := range org.Members {
		_, err := tx.ExecContext(ctx, `INSERT INTO org_members (org_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`,
			org.ID, m.UserID, string(m.Role), m.JoinedAt)
		if err != nil {
			return fmt.Errorf("insert org member in tx: %w", err)
		}
	}
	return nil
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
	// Cap the number of chunks loaded from DB to avoid OOM on large
	// knowledge bases. The cosine similarity sort and final truncation
	// still happens in Go for portability (no pgvector dependency).
	// ORDER BY doc_id, id makes the 500-chunk sample deterministic (same
	// chunks on repeated calls) — without it, Postgres returns an arbitrary
	// set of rows depending on physical layout / vacuum state.
	// TODO: migrate to pgvector for server-side similarity search.
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
