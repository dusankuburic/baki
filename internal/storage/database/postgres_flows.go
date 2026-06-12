package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/storage/interfaces"
)

// SaveFlow upserts a flow document.
func (b *PostgresStorageBackend) SaveFlow(ctx context.Context, flow *interfaces.FlowDocument) error {
	content := flow.Content
	if len(content) == 0 {
		content = []byte("{}")
	}
	meta, err := json.Marshal(flow.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	now := time.Now().UTC()
	_, err = b.db.ExecContext(ctx, `
		INSERT INTO flows (id, name, description, content, metadata, owner_id, org_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			name        = EXCLUDED.name,
			description = EXCLUDED.description,
			content     = EXCLUDED.content,
			metadata    = EXCLUDED.metadata,
			owner_id    = EXCLUDED.owner_id,
			org_id      = EXCLUDED.org_id,
			updated_at  = EXCLUDED.updated_at`,
		flow.ID, flow.Name, flow.Description, string(content), string(meta),
		flow.OwnerID, flow.OrganizationID, now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert flow: %w", err)
	}
	flow.UpdatedAt = now
	return nil
}

// LoadFlow retrieves a flow document by ID.
func (b *PostgresStorageBackend) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	row := b.db.QueryRowContext(ctx,
		`SELECT id, name, description, content, metadata, owner_id, org_id, created_at, updated_at
		 FROM flows WHERE id = $1`, id)

	var flow interfaces.FlowDocument
	var contentRaw, metaRaw []byte
	if err := row.Scan(
		&flow.ID, &flow.Name, &flow.Description,
		&contentRaw, &metaRaw,
		&flow.OwnerID, &flow.OrganizationID,
		&flow.CreatedAt, &flow.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("scan flow: %w", err)
	}
	flow.Content = contentRaw
	if err := json.Unmarshal(metaRaw, &flow.Metadata); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	return &flow, nil
}

// ListFlows returns flows matching the filter.
func (b *PostgresStorageBackend) ListFlows(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	where := []string{"1=1"}
	args := []any{}
	n := 1

	// Ownership / org filtering: show flows owned by UserID, belonging to OrganizationID,
	// or where the user is an explicit collaborator.
	if filter.UserID != "" {
		collabSubquery := fmt.Sprintf("(SELECT 1 FROM flow_collaborators WHERE flow_id = id AND user_id = $%d)", n)
		if filter.OrganizationID != "" {
			where = append(where, fmt.Sprintf("(owner_id = $%d OR org_id = $%d OR EXISTS %s)", n, n+1, collabSubquery))
			args = append(args, filter.UserID, filter.OrganizationID)
			n += 2
		} else {
			where = append(where, fmt.Sprintf("(owner_id = $%d OR EXISTS %s)", n, collabSubquery))
			args = append(args, filter.UserID)
			n++
		}
	} else if filter.OrganizationID != "" {
		where = append(where, fmt.Sprintf("org_id = $%d", n))
		args = append(args, filter.OrganizationID)
		n++
	}

	if filter.Query != "" {
		where = append(where, fmt.Sprintf("name ILIKE $%d", n))
		args = append(args, "%"+filter.Query+"%")
		n++
	}
	if filter.CreatedAfter != nil {
		where = append(where, fmt.Sprintf("created_at > $%d", n))
		args = append(args, *filter.CreatedAfter)
		n++
	}
	if filter.CreatedBefore != nil {
		where = append(where, fmt.Sprintf("created_at < $%d", n))
		args = append(args, *filter.CreatedBefore)
		n++
	}

	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit, filter.Offset)

	// Avoid shipping the (potentially large) content JSONB when the caller only
	// needs listing metadata — selecting a literal keeps the row shape identical.
	contentExpr := "content"
	if filter.MetadataOnly {
		contentExpr = "'{}'::jsonb AS content"
	}

	q := fmt.Sprintf(`
		SELECT id, name, description, %s, metadata, owner_id, org_id, created_at, updated_at
		FROM flows
		WHERE %s
		ORDER BY updated_at DESC
		LIMIT $%d OFFSET $%d`,
		contentExpr, strings.Join(where, " AND "), n, n+1)

	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list flows: %w", err)
	}
	defer rows.Close()

	var result []*interfaces.FlowDocument
	for rows.Next() {
		var flow interfaces.FlowDocument
		var contentRaw, metaRaw []byte
		if err := rows.Scan(
			&flow.ID, &flow.Name, &flow.Description,
			&contentRaw, &metaRaw,
			&flow.OwnerID, &flow.OrganizationID,
			&flow.CreatedAt, &flow.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		flow.Content = contentRaw
		if filter.MetadataOnly {
			// Keep Content nil (not the "{}" placeholder we selected) so the
			// result shape matches the filesystem backend and callers can rely
			// on "MetadataOnly ⇒ empty Content" regardless of backend.
			flow.Content = nil
		}
		if err := json.Unmarshal(metaRaw, &flow.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
		result = append(result, &flow)
	}
	return result, rows.Err()
}

// DeleteFlow removes a flow by ID.
func (b *PostgresStorageBackend) DeleteFlow(ctx context.Context, id string) error {
	res, err := b.db.ExecContext(ctx, `DELETE FROM flows WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete flow: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// SaveConversation upserts a conversation (chat history) for a flow+scope.
func (b *PostgresStorageBackend) SaveConversation(ctx context.Context, flowID, scope string, messages []interfaces.ChatMessage) error {
	data, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("marshal messages: %w", err)
	}
	_, err = b.db.ExecContext(ctx, `
		INSERT INTO conversations (flow_id, scope, messages, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (flow_id, scope) DO UPDATE SET
			messages   = EXCLUDED.messages,
			updated_at = EXCLUDED.updated_at`,
		flowID, scope, data, time.Now().UTC())
	return err
}

// LoadConversation retrieves the conversation for a flow+scope. Returns a
// non-nil empty slice when no conversation exists yet — this matches the
// filesystem backend's semantics so callers can use a single nil-safe check
// across both backends. A `nil` return therefore always indicates an error.
func (b *PostgresStorageBackend) LoadConversation(ctx context.Context, flowID, scope string) ([]interfaces.ChatMessage, error) {
	var data []byte
	err := b.db.QueryRowContext(ctx,
		`SELECT messages FROM conversations WHERE flow_id = $1 AND scope = $2`,
		flowID, scope,
	).Scan(&data)
	if err == sql.ErrNoRows {
		return []interfaces.ChatMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load conversation: %w", err)
	}
	var msgs []interfaces.ChatMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("unmarshal messages: %w", err)
	}
	if msgs == nil {
		// JSON "null" unmarshals to a nil slice; normalize to empty for parity.
		msgs = []interfaces.ChatMessage{}
	}
	return msgs, nil
}

// ListCollaborators returns the collaborators for a flow.
func (b *PostgresStorageBackend) ListCollaborators(ctx context.Context, flowID string) ([]*interfaces.Collaborator, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT c.user_id, u.email, c.permission, c.granted_at
		FROM flow_collaborators c
		JOIN users u ON c.user_id = u.id
		WHERE c.flow_id = $1
		ORDER BY c.granted_at ASC`, flowID)
	if err != nil {
		return nil, fmt.Errorf("query collaborators: %w", err)
	}
	defer rows.Close()

	// Contract: empty result is a non-nil empty slice (matches filesystem
	// backend), so callers can use `len(collabs) == 0` without backend-
	// specific nil checks. A nil return is reserved for errors.
	collabs := []*interfaces.Collaborator{}
	for rows.Next() {
		var c interfaces.Collaborator
		if err := rows.Scan(&c.UserID, &c.Email, &c.Permission, &c.GrantedAt); err != nil {
			return nil, fmt.Errorf("scan collaborator: %w", err)
		}
		collabs = append(collabs, &c)
	}
	return collabs, rows.Err()
}

func (b *PostgresStorageBackend) AddCollaborator(ctx context.Context, flowID string, c *interfaces.Collaborator) error {
	if c.GrantedAt.IsZero() {
		c.GrantedAt = time.Now().UTC()
	}
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO flow_collaborators (flow_id, user_id, permission, granted_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (flow_id, user_id) DO UPDATE SET
			permission = EXCLUDED.permission,
			granted_at = EXCLUDED.granted_at`,
		flowID, c.UserID, c.Permission, c.GrantedAt)
	if err != nil {
		return fmt.Errorf("add collaborator: %w", err)
	}
	return nil
}

func (b *PostgresStorageBackend) UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error {
	res, err := b.db.ExecContext(ctx, `
		UPDATE flow_collaborators SET permission = $1
		WHERE flow_id = $2 AND user_id = $3`,
		permission, flowID, userID)
	if err != nil {
		return fmt.Errorf("update collaborator: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

func (b *PostgresStorageBackend) RemoveCollaborator(ctx context.Context, flowID, userID string) error {
	res, err := b.db.ExecContext(ctx, `
		DELETE FROM flow_collaborators
		WHERE flow_id = $1 AND user_id = $2`,
		flowID, userID)
	if err != nil {
		return fmt.Errorf("remove collaborator: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// SaveFlowVersion persists a flow version snapshot.
func (b *PostgresStorageBackend) SaveFlowVersion(ctx context.Context, v *interfaces.FlowVersion) error {
	meta, err := json.Marshal(v.Metadata)
	if err != nil {
		meta = []byte("{}")
	}
	content := v.Content
	if content == nil {
		content = json.RawMessage("{}")
	}
	_, err = b.db.ExecContext(ctx,
		`INSERT INTO flow_versions (id, flow_id, version, comment, content, metadata, created_by, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (flow_id, version) DO NOTHING`,
		v.ID, v.FlowID, v.Version, v.Comment, content, meta, v.CreatedBy, v.CreatedAt,
	)
	return err
}

func (b *PostgresStorageBackend) ListFlowVersions(ctx context.Context, flowID string, limit int) ([]*interfaces.FlowVersion, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := b.db.QueryContext(ctx,
		`SELECT id, flow_id, version, comment, metadata, created_by, created_at
		 FROM flow_versions WHERE flow_id = $1
		 ORDER BY version DESC LIMIT $2`,
		flowID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list flow versions: %w", err)
	}
	defer rows.Close()

	var versions []*interfaces.FlowVersion
	for rows.Next() {
		v := &interfaces.FlowVersion{}
		var metaRaw []byte
		if err := rows.Scan(&v.ID, &v.FlowID, &v.Version, &v.Comment,
			&metaRaw, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		if len(metaRaw) > 0 {
			if err := json.Unmarshal(metaRaw, &v.Metadata); err != nil {
				logger.Warn("failed to parse flow metadata JSON", "error", err, "flow_id", v.ID)
			}
		}
		versions = append(versions, v)
	}
	if versions == nil {
		versions = []*interfaces.FlowVersion{}
	}
	return versions, rows.Err()
}

func (b *PostgresStorageBackend) LoadFlowVersion(ctx context.Context, flowID string, version int) (*interfaces.FlowVersion, error) {
	v := &interfaces.FlowVersion{}
	var metaRaw []byte
	err := b.db.QueryRowContext(ctx,
		`SELECT id, flow_id, version, comment, content, metadata, created_by, created_at
		 FROM flow_versions WHERE flow_id = $1 AND version = $2`,
		flowID, version,
	).Scan(&v.ID, &v.FlowID, &v.Version, &v.Comment, &v.Content, &metaRaw, &v.CreatedBy, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load flow version: %w", err)
	}
	if len(metaRaw) > 0 {
		if err := json.Unmarshal(metaRaw, &v.Metadata); err != nil {
			logger.Warn("failed to parse flow metadata JSON", "error", err, "flow_id", v.ID)
		}
	}
	return v, nil
}
