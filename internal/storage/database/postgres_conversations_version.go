package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// The conversation version (migration 0044, ADR 0031) is assigned by a trigger from one sequence: the code here
// only reads it and compares it. A save that changes the messages, blind or not, therefore gives the row a number
// no other write has had.

// LoadConversationVersioned is LoadConversation plus the row's version; a missing conversation is an empty list at
// version 0.
func (b *PostgresStorageBackend) LoadConversationVersioned(ctx context.Context, flowID, scope string) ([]interfaces.ChatMessage, int64, error) {
	var (
		data    []byte
		version int64
	)
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT messages, version FROM conversations WHERE flow_id = $1 AND scope = $2`,
		flowID, scope,
	).Scan(&data, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return []interfaces.ChatMessage{}, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("load conversation: %w", err)
	}
	var msgs []interfaces.ChatMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, 0, fmt.Errorf("unmarshal messages: %w", err)
	}
	if msgs == nil {
		msgs = []interfaces.ChatMessage{}
	}
	return msgs, version, nil
}

// SaveConversationIf is a compare-and-swap on the row's version. The UPDATE is the swap: it matches only the
// version the caller read, and under READ COMMITTED a writer that waited on the row lock re-checks the predicate
// against the winner's row, so exactly one of two writers on one base version succeeds. A create (expected 0) that
// finds no row to update inserts, and loses to a concurrent create through ON CONFLICT DO NOTHING.
func (b *PostgresStorageBackend) SaveConversationIf(ctx context.Context, flowID, scope string, expected int64, messages []interfaces.ChatMessage) (int64, error) {
	data, err := json.Marshal(messages)
	if err != nil {
		return 0, fmt.Errorf("marshal messages: %w", err)
	}
	now := time.Now().UTC()
	q := b.query(ctx)
	var version int64
	// A save of the list already stored keeps its updated_at as well as its version (the trigger leaves the version
	// alone when the messages are equal).
	err = q.QueryRowContext(ctx, `
		UPDATE conversations SET
			messages   = $3,
			updated_at = CASE WHEN messages IS DISTINCT FROM $3::jsonb THEN $4 ELSE updated_at END
		WHERE flow_id = $1 AND scope = $2 AND version = $5
		RETURNING version`,
		flowID, scope, data, now, expected).Scan(&version)
	if err == nil {
		return version, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("save conversation: %w", err)
	}
	if expected != 0 {
		return b.currentConversationVersion(ctx, flowID, scope), interfaces.ErrConversationConflict
	}
	err = q.QueryRowContext(ctx, `
		INSERT INTO conversations (flow_id, scope, messages, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (flow_id, scope) DO NOTHING
		RETURNING version`,
		flowID, scope, data, now).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return b.currentConversationVersion(ctx, flowID, scope), interfaces.ErrConversationConflict
	}
	if err != nil {
		return 0, fmt.Errorf("save conversation: %w", err)
	}
	return version, nil
}

// DeleteConversationIf removes the row only at the version the caller read. No row for expected 0 is the same
// idempotent no-op DeleteConversation is; a row at another version, or no row for a version the caller held, means
// the conversation changed since.
func (b *PostgresStorageBackend) DeleteConversationIf(ctx context.Context, flowID, scope string, expected int64) error {
	q := b.query(ctx)
	res, err := q.ExecContext(ctx,
		`DELETE FROM conversations WHERE flow_id = $1 AND scope = $2 AND version = $3`, flowID, scope, expected)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if expected != 0 {
		return interfaces.ErrConversationConflict
	}
	var exists bool
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM conversations WHERE flow_id = $1 AND scope = $2)`, flowID, scope).Scan(&exists); err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	if exists {
		return interfaces.ErrConversationConflict
	}
	return nil
}

// currentConversationVersion is the version a refused compare-and-swap reports to its caller, read after the failed
// write; 0 when the row is gone, invisible to the caller or unreadable, since the caller only uses it as a hint.
func (b *PostgresStorageBackend) currentConversationVersion(ctx context.Context, flowID, scope string) int64 {
	var version int64
	if err := b.query(ctx).QueryRowContext(ctx,
		`SELECT version FROM conversations WHERE flow_id = $1 AND scope = $2`, flowID, scope).Scan(&version); err != nil {
		return 0
	}
	return version
}
