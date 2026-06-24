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
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// maxBlobContentBytes bounds the size of a single flow-content blob. The whole
// payload is held in memory on upload (UploadBuffer) and download
// (bytes.Buffer), so a hard cap prevents an oversized flow from spiking memory.
// Flow JSON is normally well under 1 MiB; 50 MiB is a generous ceiling.
const maxBlobContentBytes = 50 << 20

// flowContentKey is the version-keyed blob key for a flow's current content.
// Keying by the DB version — the single source of truth for "which blob is
// current" — means a racing or rolled-back write leaves an orphan under an
// unreferenced version instead of clobbering the live content. The OCC version
// space (flows.version) is distinct from the snapshot space (flow_versions.version
// used by SaveFlowVersion), so this "content.vN" key never collides with a
// snapshot under "versions/N".
func flowContentKey(flowID string, version int) string {
	return fmt.Sprintf("flows/%s/content.v%d.json", flowID, version)
}

// legacyFlowContentKey is the original version-agnostic key. LoadFlow falls
// back to it so flows written before the versioned scheme remain readable.
func legacyFlowContentKey(flowID string) string {
	return fmt.Sprintf("flows/%s/content.json", flowID)
}

// blobErrStatus classifies an azblob error into a metric status label,
// distinguishing Azure throttling (429) from other failures.
func blobErrStatus(err error) string {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) && respErr.StatusCode == 429 {
		return "throttled"
	}
	return "error"
}

// SaveFlow upserts a flow document. When flow.Version > 0 and the row
// already exists, the UPDATE is conditional on the current DB version
// matching flow.Version (optimistic concurrency control). A version of 0
// skips the check for backward compatibility with callers that don't set it.
func (b *PostgresStorageBackend) SaveFlow(ctx context.Context, flow *interfaces.FlowDocument) error {
	content := flow.Content
	if len(content) == 0 {
		content = []byte("{}")
	}

	// The version bump (DB) and the content upload (blob) must commit together,
	// or a failed upload could leave the row pointing at a version whose blob
	// never landed (then LoadFlow hard-errors). Inside an RLS request the
	// surrounding transaction already provides this (it commits only after the
	// handler succeeds). Without one — e.g. BeginRLS failed and the request runs
	// in autocommit — wrap the work in a local transaction so the version bump is
	// rolled back if the upload fails.
	if b.blobClient != nil && !hasRLSTx(ctx) {
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("save flow: begin tx: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		if err := b.saveFlowTx(WithRLSTx(ctx, tx), flow, content); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("save flow: commit: %w", err)
		}
		committed = true
		return nil
	}
	return b.saveFlowTx(ctx, flow, content)
}

// saveFlowTx performs the conditional upsert and (when blob storage is
// configured) the content upload, using whatever executor b.query(ctx) resolves
// to. content is the caller's normalized content ("{}" when empty).
func (b *PostgresStorageBackend) saveFlowTx(ctx context.Context, flow *interfaces.FlowDocument, content []byte) error {
	// When blob storage is configured the content lives in the blob and the DB
	// row only holds a "{}" placeholder. Two safeguards prevent dual-write
	// corruption:
	//   1. The blob is written *after* the optimistic-concurrency check below
	//      succeeds, so a rejected concurrent writer never touches the blob.
	//   2. The blob is keyed by the *new* DB version (flowContentKey), and
	//      LoadFlow resolves content by the version stored on the row. The DB
	//      version is therefore the single source of truth for "which blob is
	//      current": if the upload succeeds but the surrounding transaction's
	//      COMMIT later fails, the row stays at the previous version and still
	//      points at the previous (correct) blob — the new blob is just an
	//      unreferenced orphan, not corruption. (Orphans are reclaimed by a
	//      storage lifecycle policy / the prefix delete in DeleteFlow.)
	dbContent := content
	if b.blobClient != nil {
		dbContent = []byte("{}")
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
		WHERE flows.version = $10
		RETURNING version`,
		flow.ID, flow.Name, flow.Description, string(dbContent), string(meta),
		flow.OwnerID, flow.OrganizationID, now, now, expectedVer,
	).Scan(&newVersion)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return interfaces.ErrVersionConflict
		}
		return fmt.Errorf("upsert flow: %w", err)
	}

	// OCC check passed — only now is it safe to persist the real content to the
	// blob, under the new version's key so a rejected concurrent write can never
	// clobber the stored content.
	if b.blobClient != nil {
		if err := b.uploadBlob(ctx, flowContentKey(flow.ID, newVersion), content); err != nil {
			return err
		}
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

// loadFlowRow reads the flow's DB row (no blob download). Content is the raw DB
// value — a "{}" placeholder when blob storage is configured.
func (b *PostgresStorageBackend) loadFlowRow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
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
	return &flow, nil
}

// LoadFlowHeader retrieves a flow's metadata (owner, org, version, …) WITHOUT
// fetching its content blob. Callers that only need to authorize, check
// existence, or read the version (authz, OCC pre-checks) must use this so they
// don't pay a blob round-trip and don't fail when blob storage is briefly
// unavailable. Content is always nil.
func (b *PostgresStorageBackend) LoadFlowHeader(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	flow, err := b.loadFlowRow(ctx, id)
	if err != nil {
		return nil, err
	}
	flow.Content = nil
	return flow, nil
}

// LoadFlow retrieves a flow document by ID, including its content (from blob
// storage when configured).
func (b *PostgresStorageBackend) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	flow, err := b.loadFlowRow(ctx, id)
	if err != nil {
		return nil, err
	}

	// Download from Blob Storage if configured and DB content is placeholder.
	if b.blobClient != nil && (len(flow.Content) == 0 || string(flow.Content) == "{}" || string(flow.Content) == "null") {
		content, err := b.downloadBlob(ctx, flowContentKey(flow.ID, flow.Version))
		if err != nil {
			// Blob storage is unreachable/unreadable. Returning a hollow flow with
			// empty content would silently corrupt the caller's view, so fail loudly.
			return nil, fmt.Errorf("load flow %s: content unavailable in blob storage: %w", flow.ID, err)
		}
		if content == nil {
			// Fall back to the legacy version-agnostic key for flows written
			// before the versioned scheme.
			content, err = b.downloadBlob(ctx, legacyFlowContentKey(flow.ID))
			if err != nil {
				return nil, fmt.Errorf("load flow %s: content unavailable in blob storage: %w", flow.ID, err)
			}
		}
		if content != nil {
			flow.Content = content
		} else if flow.Metadata.BlockCount > 0 {
			// Both keys 404 on a row whose metadata records blocks: the content
			// blob is missing though it should exist — data loss, not a
			// legitimately empty flow. Fail rather than returning a hollow {}.
			return nil, fmt.Errorf("load flow %s: content blob missing though metadata records %d block(s)", flow.ID, flow.Metadata.BlockCount)
		}
	}

	return flow, nil
}

// flowFilterWhere builds the WHERE clause and args for a FlowFilter. Shared by
// ListFlows and CountFlows so the two can never drift apart. Returns the
// joined clause, the args, and the next free placeholder index.
func flowFilterWhere(filter interfaces.FlowFilter) (string, []any, int) {
	where := []string{"1=1"}
	args := []any{}
	n := 1

	// Ownership / org filtering: show flows owned by UserID, belonging to OrganizationID(s),
	// or where the user is an explicit collaborator. Membership of UserID in
	// OrganizationID / OrganizationIDs is enforced by the service layer
	// (AuthzService) before the filter reaches storage.
	switch {
	case filter.AllFlows:
		// Explicit operational enumeration (migration): no owner/org scoping.
	case filter.SharedOnly:
		// "Shared with me" — only flows where the caller is a collaborator
		// (excluding flows they own outright). UserID is required.
		if filter.UserID == "" {
			return "1=0", nil, 1
		}
		where = append(where, fmt.Sprintf(
			"owner_id <> $%d AND EXISTS (SELECT 1 FROM flow_collaborators WHERE flow_id = id AND user_id = $%d)",
			n, n))
		args = append(args, filter.UserID)
		n++
	case filter.UserID == "" && filter.OrganizationID == "" && len(filter.OrganizationIDs) == 0:
		// Defense-in-depth: if neither UserID nor any org scoping is set the
		// filter would match every row. Return an always-false clause instead
		// so a caller who forgets to set filter fields cannot dump all data.
		return "1=0", nil, 1
	case filter.UserID != "":
		clauses := []string{fmt.Sprintf("owner_id = $%d", n)}
		args = append(args, filter.UserID)
		userArg := n
		n++
		if filter.OrganizationID != "" {
			clauses = append(clauses, fmt.Sprintf("org_id = $%d", n))
			args = append(args, filter.OrganizationID)
			n++
		}
		if len(filter.OrganizationIDs) > 0 {
			// pq array binding — postgres ANY()
			placeholders := make([]string, len(filter.OrganizationIDs))
			for i, id := range filter.OrganizationIDs {
				placeholders[i] = fmt.Sprintf("$%d", n)
				args = append(args, id)
				n++
			}
			clauses = append(clauses, fmt.Sprintf("org_id IN (%s)", strings.Join(placeholders, ",")))
		}
		clauses = append(clauses, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM flow_collaborators WHERE flow_id = id AND user_id = $%d)",
			userArg))
		where = append(where, "("+strings.Join(clauses, " OR ")+")")
	case filter.OrganizationID != "":
		where = append(where, fmt.Sprintf("org_id = $%d", n))
		args = append(args, filter.OrganizationID)
		n++
	case len(filter.OrganizationIDs) > 0:
		placeholders := make([]string, len(filter.OrganizationIDs))
		for i, id := range filter.OrganizationIDs {
			placeholders[i] = fmt.Sprintf("$%d", n)
			args = append(args, id)
			n++
		}
		where = append(where, fmt.Sprintf("org_id IN (%s)", strings.Join(placeholders, ",")))
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

// flowOrderBy returns the ORDER BY clause for a FlowSort. Whitelisted columns
// only — never interpolate user input here.
func flowOrderBy(s interfaces.FlowSort) string {
	switch s {
	case interfaces.FlowSortUpdatedAsc:
		return "ORDER BY updated_at ASC"
	case interfaces.FlowSortNameAsc:
		return "ORDER BY lower(name) ASC, updated_at DESC"
	case interfaces.FlowSortNameDesc:
		return "ORDER BY lower(name) DESC, updated_at DESC"
	case interfaces.FlowSortBlocksDesc:
		return "ORDER BY COALESCE((metadata->>'BlockCount')::int, 0) DESC, updated_at DESC"
	default:
		return "ORDER BY updated_at DESC"
	}
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

	orderClause := flowOrderBy(filter.SortBy)
	q := fmt.Sprintf(`
		SELECT id, name, description, %s, metadata, owner_id, org_id, created_at, updated_at, version
		FROM flows
		WHERE %s
		%s
		LIMIT $%d OFFSET $%d`,
		contentExpr, whereClause, orderClause, n, n+1)

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
		// Bound concurrent Azure blob downloads: a page can hold up to 500 flows
		// (see limit above), and firing that many downloads at once risks Azure
		// throttling (429s) and a memory spike holding every body in flight.
		// g.Go then blocks until a slot frees, so fetches still parallelize.
		// 16 matches the house cap in internal/ai (maxConcurrentRecords).
		g.SetLimit(16)
		for _, flow := range result {
			if len(flow.Content) == 0 || string(flow.Content) == "{}" || string(flow.Content) == "null" {
				f := flow // capture range variable
				g.Go(func() (err error) {
					// errgroup does not recover panics raised in its goroutines, so
					// a panic in the azblob SDK would crash the process. Recover and
					// degrade to "no content" for this one flow, matching the
					// log-and-continue behaviour used for download errors below.
					defer func() {
						if r := recover(); r != nil {
							logger.Warn("ListFlows blob fetch goroutine panicked", "flow_id", f.ID, "err", r)
							err = nil
						}
					}()
					content, err := b.downloadBlob(gctx, flowContentKey(f.ID, f.Version))
					if err != nil {
						logger.Warn("failed to download flow content in list", "flow_id", f.ID, "error", err)
						return nil // log and continue to avoid failing the entire list
					}
					if content == nil {
						// Fall back to the legacy version-agnostic key.
						content, err = b.downloadBlob(gctx, legacyFlowContentKey(f.ID))
						if err != nil {
							logger.Warn("failed to download legacy flow content in list", "flow_id", f.ID, "error", err)
							return nil
						}
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

	// Delete all blobs related to this flow (content + versions). Defer the
	// cleanup until the DB delete is durably committed: deleting blobs while the
	// surrounding RLS transaction is still open would orphan a surviving row's
	// content if the request later rolls back. RegisterPostCommit runs the hook
	// after commit, or immediately when there is no RLS tx (autocommit — the
	// delete is already durable).
	if b.blobClient != nil {
		prefix := fmt.Sprintf("flows/%s/", id)
		RegisterPostCommit(ctx, func() {
			// #nosec G118 -- intentionally detached with its own bounded timeout so
			// a slow/hung blob store can't block request teardown or leak.
			go func() {
				// Recover: a panic in the azblob SDK would otherwise crash the
				// process (project convention for background goroutines).
				defer func() {
					if r := recover(); r != nil {
						logger.Warn("DeleteFlow blob cleanup goroutine panicked", "flow_id", id, "err", r)
					}
				}()
				cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := b.deleteBlobs(cctx, prefix); err != nil {
					logger.Warn("failed to delete flow blobs", "flow_id", id, "error", err)
				}
			}()
		})
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

// DeleteConversation removes the conversation for a flow+scope. Deleting a
// missing conversation is a no-op (matches the filesystem backend's idempotent
// clear), so callers can use it as a "clear history" operation.
func (b *PostgresStorageBackend) DeleteConversation(ctx context.Context, flowID, scope string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM conversations WHERE flow_id = $1 AND scope = $2`, flowID, scope)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	return nil
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
	if len(data) > maxBlobContentBytes {
		return fmt.Errorf("upload blob %s: content size %d bytes exceeds limit of %d bytes", key, len(data), maxBlobContentBytes)
	}
	start := time.Now()
	_, err := b.blobClient.UploadBuffer(ctx, b.container, key, data, &azblob.UploadBufferOptions{})
	if err != nil {
		metrics.RecordBlobOp("upload", blobErrStatus(err), time.Since(start))
		return fmt.Errorf("upload blob %s: %w", key, err)
	}
	metrics.RecordBlobOp("upload", "ok", time.Since(start))
	return nil
}

func (b *PostgresStorageBackend) downloadBlob(ctx context.Context, key string) ([]byte, error) {
	if b.blobClient == nil {
		return nil, nil
	}
	start := time.Now()
	resp, err := b.blobClient.DownloadStream(ctx, b.container, key, &azblob.DownloadStreamOptions{})
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			metrics.RecordBlobOp("download", "not_found", time.Since(start))
			return nil, nil
		}
		metrics.RecordBlobOp("download", blobErrStatus(err), time.Since(start))
		return nil, fmt.Errorf("download blob %s: %w", key, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		metrics.RecordBlobOp("download", "error", time.Since(start))
		return nil, fmt.Errorf("read blob %s: %w", key, err)
	}
	metrics.RecordBlobOp("download", "ok", time.Since(start))
	return buf.Bytes(), nil
}

// maxBlobBatchOps is the Azure Blob Batch limit of sub-requests per batch.
const maxBlobBatchOps = 256

func (b *PostgresStorageBackend) deleteBlobs(ctx context.Context, prefix string) error {
	if b.blobClient == nil {
		return nil
	}
	svc := b.blobClient.ServiceClient()
	pager := b.blobClient.NewListBlobsFlatPager(b.container, &azblob.ListBlobsFlatOptions{
		Prefix: &prefix,
	})

	var failed int
	names := make([]string, 0, maxBlobBatchOps)

	// flush deletes the accumulated blob names in a single Blob Batch request
	// (one round trip for up to 256 blobs instead of one DELETE each).
	flush := func() error {
		if len(names) == 0 {
			return nil
		}
		start := time.Now()
		bb, err := svc.NewBatchBuilder()
		if err != nil {
			metrics.RecordBlobOp("delete", "error", time.Since(start))
			return fmt.Errorf("create blob delete batch: %w", err)
		}
		for _, name := range names {
			if err := bb.Delete(b.container, name, nil); err != nil {
				metrics.RecordBlobOp("delete", "error", time.Since(start))
				return fmt.Errorf("queue blob %q for batch delete: %w", name, err)
			}
		}
		resp, err := svc.SubmitBatch(ctx, bb, nil)
		if err != nil {
			metrics.RecordBlobOp("delete", blobErrStatus(err), time.Since(start))
			return fmt.Errorf("submit blob delete batch: %w", err)
		}
		// The batch call succeeded as a whole; individual sub-requests can still
		// fail (e.g. a blob deleted concurrently). Count those but don't abort —
		// they're surfaced via the returned error so leaks stay observable.
		batchFailed := 0
		for _, item := range resp.Responses {
			if item != nil && item.Error != nil {
				batchFailed++
				name := ""
				if item.BlobName != nil {
					name = *item.BlobName
				}
				logger.Warn("failed to delete blob in batch", "key", name, "error", item.Error)
			}
		}
		failed += batchFailed
		status := "ok"
		if batchFailed > 0 {
			status = "error"
		}
		metrics.RecordBlobOp("delete", status, time.Since(start))
		names = names[:0]
		return nil
	}

	for pager.More() {
		start := time.Now()
		page, err := pager.NextPage(ctx)
		if err != nil {
			metrics.RecordBlobOp("list", blobErrStatus(err), time.Since(start))
			return fmt.Errorf("list blobs for deletion: %w", err)
		}
		metrics.RecordBlobOp("list", "ok", time.Since(start))
		for _, blob := range page.Segment.BlobItems {
			if blob.Name == nil {
				continue
			}
			names = append(names, *blob.Name)
			if len(names) == maxBlobBatchOps {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	// Surface partial failures so the caller (and its metric/log) can see that
	// blobs leaked, rather than silently returning nil.
	if failed > 0 {
		return fmt.Errorf("delete blobs under %q: %d blob(s) failed to delete", prefix, failed)
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

	// Download from Blob Storage if configured and DB content is placeholder.
	if b.blobClient != nil && (len(v.Content) == 0 || string(v.Content) == "{}" || string(v.Content) == "null") {
		blobKey := fmt.Sprintf("flows/%s/versions/%d/content.json", v.FlowID, v.Version)
		content, err := b.downloadBlob(ctx, blobKey)
		if err != nil {
			return nil, fmt.Errorf("load flow version %s/%d: content unavailable in blob storage: %w", v.FlowID, v.Version, err)
		}
		if content != nil {
			v.Content = content
		} else if v.Metadata.BlockCount > 0 {
			return nil, fmt.Errorf("load flow version %s/%d: content blob missing though metadata records %d block(s)", v.FlowID, v.Version, v.Metadata.BlockCount)
		}
	}

	return v, nil
}
