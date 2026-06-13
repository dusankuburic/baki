package database

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"golang.org/x/sync/errgroup"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/storage/interfaces"
)

// SaveFlow upserts a flow document. When flow.Version > 0 and the row
// already exists, the UPDATE is conditional on the current DB version
// matching flow.Version (optimistic concurrency control). A version of 0
// skips the check for backward compatibility with callers that don't set it.
func (b *PostgresStorageBackend) SaveFlow(ctx context.Context, flow *interfaces.FlowDocument) error {
	content := flow.Content
	if len(content) == 0 {
		content = []byte("{}")
	}

	// Upload to Blob Storage if configured
	if b.blobClient != nil {
		blobKey := fmt.Sprintf("flows/%s/content.json", flow.ID)
		if err := b.uploadBlob(ctx, blobKey, content); err != nil {
			return err
		}
		// Clear content for DB storage to save space
		content = []byte("{}")
	}

	meta, err := json.Marshal(flow.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	now := time.Now().UTC()
	expectedVer := flow.Version
	var newVersion int
	err = b.query(ctx).QueryRowContext(ctx, `
		INSERT INTO flows (id, name, description, content, metadata, owner_id, org_id, created_at, updated_at, version)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8, $9, 0)
		ON CONFLICT (id) DO UPDATE SET
			name        = EXCLUDED.name,
			description = EXCLUDED.description,
			content     = EXCLUDED.content,
			metadata    = EXCLUDED.metadata,
			updated_at  = EXCLUDED.updated_at,
			version     = flows.version + 1
		WHERE flows.version = $10 OR $10 = 0
		RETURNING version`,
		flow.ID, flow.Name, flow.Description, string(content), string(meta),
		flow.OwnerID, flow.OrganizationID, now, now, expectedVer,
	).Scan(&newVersion)
	if err != nil {
		if err == sql.ErrNoRows {
			return interfaces.ErrVersionConflict
		}
		return fmt.Errorf("upsert flow: %w", err)
	}
	flow.UpdatedAt = now
	flow.Version = newVersion
	return nil
}

func (b *PostgresStorageBackend) TransferFlowOwner(ctx context.Context, flowID, newOwnerID, newOrgID string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE flows SET owner_id = $1, org_id = $2, updated_at = $3 WHERE id = $4`,
		newOwnerID, newOrgID, time.Now().UTC(), flowID)
	if err != nil {
		return fmt.Errorf("transfer flow owner: %w", err)
	}
	return nil
}

// LoadFlow retrieves a flow document by ID.
func (b *PostgresStorageBackend) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	row := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, name, description, content, metadata, owner_id, org_id, created_at, updated_at, version
		 FROM flows WHERE id = $1`, id)

	var flow interfaces.FlowDocument
	var contentRaw, metaRaw []byte
	if err := row.Scan(
		&flow.ID, &flow.Name, &flow.Description,
		&contentRaw, &metaRaw,
		&flow.OwnerID, &flow.OrganizationID,
		&flow.CreatedAt, &flow.UpdatedAt, &flow.Version,
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

	// Download from Blob Storage if configured and DB content is placeholder
	if b.blobClient != nil && (len(flow.Content) == 0 || string(flow.Content) == "{}" || string(flow.Content) == "null") {
		blobKey := fmt.Sprintf("flows/%s/content.json", flow.ID)
		content, err := b.downloadBlob(ctx, blobKey)
		if err != nil {
			// If not found in blob, we log and proceed (maybe it's old data)
			logger.Warn("failed to download flow content from blob", "flow_id", flow.ID, "error", err)
		} else if content != nil {
			flow.Content = content
		}
	}

	return &flow, nil
}

// flowFilterWhere builds the WHERE clause and args for a FlowFilter. Shared by
// ListFlows and CountFlows so the two can never drift apart. Returns the
// joined clause, the args, and the next free placeholder index.
func flowFilterWhere(filter interfaces.FlowFilter) (string, []any, int) {
	where := []string{"1=1"}
	args := []any{}
	n := 1

	// Ownership / org filtering: show flows owned by UserID, belonging to OrganizationID,
	// or where the user is an explicit collaborator. Membership of UserID in
	// OrganizationID is enforced by the service layer (AuthzService) before the
	// filter reaches storage.
	switch {
	case filter.AllFlows:
		// Explicit operational enumeration (migration): no owner/org scoping.
	case filter.UserID == "" && filter.OrganizationID == "":
		// Defense-in-depth: if neither UserID nor OrganizationID is set the
		// filter would match every row. Return an always-false clause instead
		// so a caller who forgets to set filter fields cannot dump all data.
		return "1=0", nil, 1
	case filter.UserID != "":
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
	case filter.OrganizationID != "":
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

	return strings.Join(where, " AND "), args, n
}

// CountFlows returns the total number of flows matching the filter, ignoring
// Limit/Offset.
func (b *PostgresStorageBackend) CountFlows(ctx context.Context, filter interfaces.FlowFilter) (int, error) {
	whereClause, args, _ := flowFilterWhere(filter)
	var total int
	err := b.query(ctx).QueryRowContext(ctx,
		fmt.Sprintf("SELECT count(*) FROM flows WHERE %s", whereClause), args...,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count flows: %w", err)
	}
	return total, nil
}

// ListFlows returns flows matching the filter.
func (b *PostgresStorageBackend) ListFlows(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	whereClause, args, n := flowFilterWhere(filter)

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
		SELECT id, name, description, %s, metadata, owner_id, org_id, created_at, updated_at, version
		FROM flows
		WHERE %s
		ORDER BY updated_at DESC
		LIMIT $%d OFFSET $%d`,
		contentExpr, whereClause, n, n+1)

	rows, err := b.query(ctx).QueryContext(ctx, q, args...)
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
			&flow.CreatedAt, &flow.UpdatedAt, &flow.Version,
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

	// Concurrent blob fetching if MetadataOnly is false and blobClient is configured
	if !filter.MetadataOnly && b.blobClient != nil {
		g, gctx := errgroup.WithContext(ctx)
		for _, flow := range result {
			if len(flow.Content) == 0 || string(flow.Content) == "{}" || string(flow.Content) == "null" {
				f := flow // capture range variable
				g.Go(func() error {
					blobKey := fmt.Sprintf("flows/%s/content.json", f.ID)
					content, err := b.downloadBlob(gctx, blobKey)
					if err != nil {
						logger.Warn("failed to download flow content in list", "flow_id", f.ID, "error", err)
						return nil // log and continue to avoid failing the entire list
					}
					if content != nil {
						f.Content = content
					}
					return nil
				})
			}
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
	}

	return result, rows.Err()
}

// DeleteFlow removes a flow by ID.
func (b *PostgresStorageBackend) DeleteFlow(ctx context.Context, id string) error {
	res, err := b.query(ctx).ExecContext(ctx, `DELETE FROM flows WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete flow: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return interfaces.ErrNotFound
	}

	// Delete all blobs related to this flow (content + versions)
	if b.blobClient != nil {
		prefix := fmt.Sprintf("flows/%s/", id)
		go func() {
			// Use a background context as the request context might be cancelled
			if err := b.deleteBlobs(context.Background(), prefix); err != nil {
				logger.Warn("failed to delete flow blobs", "flow_id", id, "error", err)
			}
		}()
	}

	return nil
}

// SaveConversation upserts a conversation (chat history) for a flow+scope.
func (b *PostgresStorageBackend) SaveConversation(ctx context.Context, flowID, scope string, messages []interfaces.ChatMessage) error {
	data, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("marshal messages: %w", err)
	}
	_, err = b.query(ctx).ExecContext(ctx, `
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
	err := b.query(ctx).QueryRowContext(ctx,
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
	rows, err := b.query(ctx).QueryContext(ctx, `
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

// ListCollaboratorsBatch returns collaborators for multiple flows in a single
// query. The map keys are flow IDs; flows with no collaborators are absent
// from the map (callers should treat missing keys as an empty list).
func (b *PostgresStorageBackend) ListCollaboratorsBatch(ctx context.Context, flowIDs []string) (map[string][]*interfaces.Collaborator, error) {
	result := make(map[string][]*interfaces.Collaborator, len(flowIDs))
	if len(flowIDs) == 0 {
		return result, nil
	}
	seen := make(map[string]bool, len(flowIDs))
	placeholders := make([]string, 0, len(flowIDs))
	args := make([]any, 0, len(flowIDs))
	for _, id := range flowIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	if len(args) == 0 {
		return result, nil
	}
	q := `SELECT c.flow_id, c.user_id, u.email, c.permission, c.granted_at
		FROM flow_collaborators c
		JOIN users u ON c.user_id = u.id
		WHERE c.flow_id IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY c.granted_at ASC`
	rows, err := b.query(ctx).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query collaborators batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var flowID string
		var c interfaces.Collaborator
		if err := rows.Scan(&flowID, &c.UserID, &c.Email, &c.Permission, &c.GrantedAt); err != nil {
			return nil, fmt.Errorf("scan collaborator batch: %w", err)
		}
		result[flowID] = append(result[flowID], &c)
	}
	return result, rows.Err()
}

func (b *PostgresStorageBackend) uploadBlob(ctx context.Context, key string, data []byte) error {
	if b.blobClient == nil {
		return nil
	}
	_, err := b.blobClient.UploadBuffer(ctx, b.container, key, data, &azblob.UploadBufferOptions{})
	if err != nil {
		return fmt.Errorf("upload blob %s: %w", key, err)
	}
	return nil
}

func (b *PostgresStorageBackend) downloadBlob(ctx context.Context, key string) ([]byte, error) {
	if b.blobClient == nil {
		return nil, nil
	}
	resp, err := b.blobClient.DownloadStream(ctx, b.container, key, &azblob.DownloadStreamOptions{})
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil, nil
		}
		return nil, fmt.Errorf("download blob %s: %w", key, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", key, err)
	}
	return buf.Bytes(), nil
}

func (b *PostgresStorageBackend) deleteBlobs(ctx context.Context, prefix string) error {
	if b.blobClient == nil {
		return nil
	}
	pager := b.blobClient.NewListBlobsFlatPager(b.container, &azblob.ListBlobsFlatOptions{
		Prefix: &prefix,
	})
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list blobs for deletion: %w", err)
		}
		for _, blob := range resp.Segment.BlobItems {
			_, err := b.blobClient.DeleteBlob(ctx, b.container, *blob.Name, &azblob.DeleteBlobOptions{})
			if err != nil {
				logger.Warn("failed to delete blob", "key", *blob.Name, "error", err)
			}
		}
	}
	return nil
}

func (b *PostgresStorageBackend) AddCollaborator(ctx context.Context, flowID string, c *interfaces.Collaborator) error {
	if c.GrantedAt.IsZero() {
		c.GrantedAt = time.Now().UTC()
	}
	_, err := b.query(ctx).ExecContext(ctx, `
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
	res, err := b.query(ctx).ExecContext(ctx, `
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
	res, err := b.query(ctx).ExecContext(ctx, `
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

	// Upload to Blob Storage if configured
	if b.blobClient != nil {
		blobKey := fmt.Sprintf("flows/%s/versions/%d/content.json", v.FlowID, v.Version)
		if err := b.uploadBlob(ctx, blobKey, content); err != nil {
			return err
		}
		// Clear content for DB storage
		content = json.RawMessage("{}")
	}

	_, err = b.query(ctx).ExecContext(ctx,
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
	rows, err := b.query(ctx).QueryContext(ctx,
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
	err := b.query(ctx).QueryRowContext(ctx,
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

	// Download from Blob Storage if configured and DB content is placeholder
	if b.blobClient != nil && (len(v.Content) == 0 || string(v.Content) == "{}" || string(v.Content) == "null") {
		blobKey := fmt.Sprintf("flows/%s/versions/%d/content.json", v.FlowID, v.Version)
		content, err := b.downloadBlob(ctx, blobKey)
		if err != nil {
			logger.Warn("failed to download flow version content from blob", "flow_id", v.FlowID, "version", v.Version, "error", err)
		} else if content != nil {
			v.Content = content
		}
	}

	return v, nil
}
