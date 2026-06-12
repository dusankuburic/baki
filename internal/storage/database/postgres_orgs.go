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
	row := b.db.QueryRowContext(ctx,
		`SELECT id, name, owner_id, created_at, updated_at FROM organisations WHERE id = $1`, id)

	var org interfaces.Organisation
	if err := row.Scan(&org.ID, &org.Name, &org.OwnerID, &org.CreatedAt, &org.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("scan org: %w", err)
	}

	rows, err := b.db.QueryContext(ctx,
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
	rows, err := b.db.QueryContext(ctx, `
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
	_, err := b.db.ExecContext(ctx, `DELETE FROM organisations WHERE id = $1`, id)
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
		if err := rows.Scan(&m.UserID, &m.Role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan org member: %w", err)
		}
		org.Members = append(org.Members, m)
	}
	return &org, nil
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
			org.ID, m.UserID, m.Role, m.JoinedAt)
		if err != nil {
			return fmt.Errorf("insert org member in tx: %w", err)
		}
	}
	return nil
}

// ---- Knowledge base operations ----

func (b *PostgresStorageBackend) SaveKnowledgeDocument(ctx context.Context, doc *interfaces.KnowledgeDocument) error {
	_, err := b.db.ExecContext(ctx, `INSERT INTO knowledge_documents (id, org_id, filename) VALUES ($1, $2, $3)`,
		doc.ID, doc.OrgID, doc.Filename)
	return err
}

func (b *PostgresStorageBackend) DeleteKnowledgeDocument(ctx context.Context, orgID, id string) error {
	_, err := b.db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1 AND org_id = $2`, id, orgID)
	return err
}

func (b *PostgresStorageBackend) ListKnowledgeDocuments(ctx context.Context, orgID string) ([]*interfaces.KnowledgeDocument, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, org_id, filename, created_at FROM knowledge_documents WHERE org_id = $1`, orgID)
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
	return docs, nil
}

func (b *PostgresStorageBackend) SaveKnowledgeChunks(ctx context.Context, chunks []interfaces.KnowledgeChunk) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO knowledge_chunks (id, doc_id, content, embedding) VALUES ($1, $2, $3, $4)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, c := range chunks {
		embJSON, _ := json.Marshal(c.Embedding)
		if _, err := stmt.ExecContext(ctx, c.ID, c.DocID, c.Content, embJSON); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (b *PostgresStorageBackend) SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	// Fetch all chunks for the org and calculate similarity in Go for portability.
	// In a real production app with millions of chunks, we'd use pgvector.
	rows, err := b.db.QueryContext(ctx, `
		SELECT c.id, c.doc_id, c.content, c.embedding 
		FROM knowledge_chunks c
		JOIN knowledge_documents d ON c.doc_id = d.id
		WHERE d.org_id = $1`, orgID)
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
		json.Unmarshal(embJSON, &c.Embedding)
		chunks = append(chunks, c)
	}

	// Score each chunk once, then sort by score. Computing similarity inside the
	// comparator would recompute each chunk's score O(n log n) times.
	type scored struct {
		chunk interfaces.KnowledgeChunk
		sim   float32
	}
	scoredChunks := make([]scored, len(chunks))
	for i, c := range chunks {
		scoredChunks[i] = scored{chunk: c, sim: cosineSimilarity(c.Embedding, queryEmbedding)}
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
