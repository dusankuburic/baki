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
//
// The owner (migration 0052, R-security-3) is named by every statement here, not left to row-level security: the
// chat routes run outside the request transaction (route.NoTx), so no row-level-security user is set on them. The
// empty owner is the system (and the deployment without accounts), which reaches every row; a user reaches their
// own rows and the unowned ones. ownerReach is that predicate, its parameter the owner.
const ownerReach = `(%[1]s = '' OR owner_id IS NULL OR owner_id = %[1]s)`

// claimable is the SQL of "this unowned row is a thread a write may claim" (interfaces.ConversationStore): the key is
// a thread id (%[1]s, interfaces.IsThreadID, decided in Go) and no stored message names it as its context block (a
// block-scoped chat's legacy key is a uuid too). Read on the row as it was: in an UPDATE's SET list `messages` is the
// old value. jsonb containment, so the (TOASTed) list is not unpacked into rows.
const claimable = `(owner_id IS NULL AND %[1]s AND NOT (messages @> jsonb_build_array(jsonb_build_object('contextBlockId', scope))))`

// LoadConversationVersioned is LoadConversation plus the row's version, as owner sees it; a missing conversation, or
// another user's, is an empty list at version 0.
func (b *PostgresStorageBackend) LoadConversationVersioned(ctx context.Context, flowID, scope, owner string) ([]interfaces.ChatMessage, int64, error) {
	var (
		data    []byte
		version int64
	)
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT messages, version FROM conversations WHERE flow_id = $1 AND scope = $2 AND `+fmt.Sprintf(ownerReach, "$3"),
		flowID, scope, owner,
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
// finds no row to update inserts, owned by owner, and loses to a concurrent create through ON CONFLICT DO NOTHING.
// The owner is in the UPDATE's predicate, so another user's row is never written; a refused write that finds such a
// row under the key answers ErrConversationKeyTaken. A claimable unowned row becomes owner's in the same UPDATE.
func (b *PostgresStorageBackend) SaveConversationIf(ctx context.Context, flowID, scope, owner string, expected int64, messages []interfaces.ChatMessage) (int64, error) {
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
			updated_at = CASE WHEN messages IS DISTINCT FROM $3::jsonb THEN $4 ELSE updated_at END,
			owner_id   = CASE WHEN `+fmt.Sprintf(claimable, "$7")+` THEN NULLIF($6, '') ELSE owner_id END
		WHERE flow_id = $1 AND scope = $2 AND version = $5 AND `+fmt.Sprintf(ownerReach, "$6")+`
		RETURNING version`,
		flowID, scope, data, now, expected, owner, owner != "" && interfaces.IsThreadID(scope)).Scan(&version)
	if err == nil {
		return version, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("save conversation: %w", err)
	}
	if expected == 0 {
		err = q.QueryRowContext(ctx, `
			INSERT INTO conversations (flow_id, scope, messages, updated_at, created_at, owner_id)
			VALUES ($1, $2, $3, $4, $4, NULLIF($5, ''))
			ON CONFLICT (flow_id, scope) DO NOTHING
			RETURNING version`,
			flowID, scope, data, now, owner).Scan(&version)
		if err == nil {
			return version, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("save conversation: %w", err)
		}
	}
	current, taken := b.currentConversationVersion(ctx, flowID, scope, owner)
	if taken {
		return 0, interfaces.ErrConversationKeyTaken
	}
	return current, interfaces.ErrConversationConflict
}

// DeleteConversationIf removes the row only at the version the caller read, and only when it is the caller's, a
// claimable unowned one, or (with mayDeleteUnowned) a shared one. No row for expected 0 is the same idempotent no-op
// DeleteConversation is, and another user's row is no row; a row at another version, or no row for a version the
// caller held, means the conversation changed since.
func (b *PostgresStorageBackend) DeleteConversationIf(ctx context.Context, flowID, scope, owner string, expected int64, mayDeleteUnowned bool) error {
	q := b.query(ctx)
	threadID := owner != "" && interfaces.IsThreadID(scope)
	res, err := q.ExecContext(ctx, `
		DELETE FROM conversations
		WHERE flow_id = $1 AND scope = $2 AND version = $3
		  AND ($4 = '' OR owner_id = $4 OR (owner_id IS NULL AND $5) OR `+fmt.Sprintf(claimable, "$6")+`)`,
		flowID, scope, expected, owner, mayDeleteUnowned, threadID)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var (
		rowOwner sql.NullString
		claim    bool
	)
	err = q.QueryRowContext(ctx,
		`SELECT owner_id, `+fmt.Sprintf(claimable, "$3")+` FROM conversations WHERE flow_id = $1 AND scope = $2`,
		flowID, scope, threadID).Scan(&rowOwner, &claim)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("delete conversation: %w", err)
	case owner != "" && rowOwner.Valid && rowOwner.String != owner:
		// Another user's: missing to this caller.
	case owner != "" && !rowOwner.Valid && !claim && !mayDeleteUnowned:
		return interfaces.ErrConversationUnowned
	default:
		return interfaces.ErrConversationConflict
	}
	if expected != 0 {
		return interfaces.ErrConversationConflict
	}
	return nil
}

// currentConversationVersion is the version a refused compare-and-swap reports to its caller, read after the failed
// write; 0 when the row is gone or unreadable, since the caller only uses it as a hint. taken says the row is another
// user's (never the system's view): its version is not the caller's to know.
func (b *PostgresStorageBackend) currentConversationVersion(ctx context.Context, flowID, scope, owner string) (version int64, taken bool) {
	var rowOwner sql.NullString
	if err := b.query(ctx).QueryRowContext(ctx,
		`SELECT version, owner_id FROM conversations WHERE flow_id = $1 AND scope = $2`, flowID, scope).Scan(&version, &rowOwner); err != nil {
		return 0, false
	}
	if owner != "" && rowOwner.Valid && rowOwner.String != owner {
		return 0, true
	}
	return version, false
}

// IsSharedConversation reports whether an unowned row that no write claims is stored under the key.
func (b *PostgresStorageBackend) IsSharedConversation(ctx context.Context, flowID, scope string) (bool, error) {
	var shared bool
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT owner_id IS NULL AND NOT `+fmt.Sprintf(claimable, "$3")+` FROM conversations WHERE flow_id = $1 AND scope = $2`,
		flowID, scope, interfaces.IsThreadID(scope)).Scan(&shared)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read conversation owner: %w", err)
	}
	return shared, nil
}

// ListConversations is owner's conversations on the flow, newest change first, keyset-paged on (updated_at, scope)
// descending: conversations_owner_idx (built by SchemaUpkeep, v52) serves it. message_count is the trigger's (v52);
// a row it has not counted yet is counted here.
func (b *PostgresStorageBackend) ListConversations(ctx context.Context, flowID, owner string, after *interfaces.PageCursor, limit int) ([]interfaces.ConversationSummary, error) {
	if owner == "" || limit <= 0 {
		return nil, nil
	}
	query := `SELECT scope, COALESCE(title, ''), COALESCE(created_at, updated_at), updated_at,
			COALESCE(message_count, CASE WHEN jsonb_typeof(messages) = 'array' THEN jsonb_array_length(messages) ELSE 0 END), version
		FROM conversations
		WHERE owner_id = $1 AND flow_id = $2`
	args := []any{owner, flowID}
	if after != nil {
		query += ` AND (updated_at, scope) < ($3, $4)`
		args = append(args, after.At, after.ID)
	}
	query += fmt.Sprintf(` ORDER BY updated_at DESC, scope DESC LIMIT %d`, limit)
	rows, err := b.query(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	out := []interfaces.ConversationSummary{}
	for rows.Next() {
		var c interfaces.ConversationSummary
		if err := rows.Scan(&c.Scope, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.MessageCount, &c.Version); err != nil {
			return nil, fmt.Errorf("list conversations: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetConversationTitle names owner's conversation (claiming a claimable unowned one), creating it empty when a thread
// id has none, and returns its version; the version trigger leaves the version alone (the messages do not change),
// and the statement leaves updated_at. A key that holds someone else's conversation, or a shared one, is refused; a
// missing one under a key that is not a thread id is ErrNotFound. A row deleted between the update and the insert is
// tried again.
func (b *PostgresStorageBackend) SetConversationTitle(ctx context.Context, flowID, scope, owner, title string) (int64, error) {
	if owner == "" {
		return 0, interfaces.ErrConversationUnowned
	}
	q := b.query(ctx)
	threadID := interfaces.IsThreadID(scope)
	for attempt := 0; attempt < 3; attempt++ {
		var version int64
		err := q.QueryRowContext(ctx, `
			UPDATE conversations SET title = NULLIF($4, ''), owner_id = $3
			WHERE flow_id = $1 AND scope = $2 AND (owner_id = $3 OR `+fmt.Sprintf(claimable, "$5")+`)
			RETURNING version`,
			flowID, scope, owner, title, threadID).Scan(&version)
		if err == nil {
			return version, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("name conversation: %w", err)
		}
		if threadID {
			now := time.Now().UTC()
			err = q.QueryRowContext(ctx, `
				INSERT INTO conversations (flow_id, scope, messages, updated_at, created_at, owner_id, title)
				VALUES ($1, $2, '[]', $3, $3, $4, NULLIF($5, ''))
				ON CONFLICT (flow_id, scope) DO NOTHING
				RETURNING version`,
				flowID, scope, now, owner, title).Scan(&version)
			if err == nil {
				return version, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("name conversation: %w", err)
			}
		}
		var rowOwner sql.NullString
		err = q.QueryRowContext(ctx,
			`SELECT owner_id FROM conversations WHERE flow_id = $1 AND scope = $2`, flowID, scope).Scan(&rowOwner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if !threadID {
				return 0, interfaces.ErrNotFound
			}
			continue // deleted in between: name it again
		case err != nil:
			return 0, fmt.Errorf("name conversation: %w", err)
		case !rowOwner.Valid:
			return 0, interfaces.ErrConversationUnowned
		case rowOwner.String != owner:
			return 0, interfaces.ErrConversationKeyTaken
		}
	}
	return 0, fmt.Errorf("name conversation: %w", interfaces.ErrConversationConflict)
}
