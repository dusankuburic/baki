package database

import (
	"context"
	"time"

	"github.com/google/uuid"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// AddFindingComment inserts a new comment. The ID and CreatedAt are stamped
// here so callers don't need to.
func (b *PostgresStorageBackend) AddFindingComment(ctx context.Context, c *storageif.FindingComment) error {
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	_, err := b.query(ctx).ExecContext(ctx,
		`INSERT INTO finding_comments (id, flow_id, finding_key, author_id, author_name, body, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		c.ID, c.FlowID, c.FindingKey, c.AuthorID, c.AuthorName, c.Body, c.CreatedAt)
	return err
}

func (b *PostgresStorageBackend) ListFindingComments(ctx context.Context, flowID, findingKey string) ([]*storageif.FindingComment, error) {
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT id, flow_id, finding_key, author_id, author_name, body, created_at
		 FROM finding_comments
		 WHERE flow_id = $1 AND finding_key = $2
		 ORDER BY created_at ASC`, flowID, findingKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storageif.FindingComment
	for rows.Next() {
		c := &storageif.FindingComment{}
		if err := rows.Scan(&c.ID, &c.FlowID, &c.FindingKey, &c.AuthorID, &c.AuthorName, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (b *PostgresStorageBackend) DeleteFindingComment(ctx context.Context, flowID, commentID, authorID string) error {
	res, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM finding_comments
		 WHERE flow_id = $1 AND id = $2 AND ($3 = '' OR author_id = $3)`,
		flowID, commentID, authorID)
	if err != nil {
		return err
	}
	if authorID == "" {
		return nil
	}
	// Nothing deleted: distinguish an absent comment (idempotent no-op) from
	// one authored by someone else (denied).
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		var exists bool
		if err := b.query(ctx).QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM finding_comments WHERE flow_id = $1 AND id = $2)`,
			flowID, commentID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return storageif.ErrNotCommentAuthor
		}
	}
	return nil
}
