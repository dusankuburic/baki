package database

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
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

// validFlowID rejects flow IDs that could escape their blob key namespace.
// Blob keys and DeleteFlow's prefix delete are built by interpolating the ID
// into "flows/{id}/..." — an ID containing '/' (e.g. "victim/versions/3")
// would nest one flow's blobs inside another's prefix, letting its owner
// delete or shadow the other flow's content. All legitimate IDs are
// server-generated UUIDs; the filesystem backend enforces the same invariant
// via its path-traversal guard.
func validFlowID(id string) error {
	if id == "" {
		return errors.New("flow id must not be empty")
	}
	if strings.Contains(id, "/") {
		return fmt.Errorf("invalid flow id %q: must not contain '/'", id)
	}
	return nil
}

// metadataRecordsContent reports whether a flow's metadata implies non-empty
// content, i.e. a missing content blob is data loss rather than a legitimately
// empty flow.
func metadataRecordsContent(m interfaces.FlowMetadata) bool {
	return m.BlockCount > 0 || m.SubflowCount > 0 || m.FileSize > 0 || m.RawLineCount > 0
}

// SaveFlow upserts a flow document. Existing rows update only when
// flow.Version matches the current DB version (optimistic concurrency
// control) — a stale version returns ErrVersionConflict, with no "skip"
// value; both backends enforce this (see the contract suite). New rows
// insert at version 0 regardless of flow.Version.
func (b *PostgresStorageBackend) SaveFlow(ctx context.Context, flow *interfaces.FlowDocument) error {
	if err := validFlowID(flow.ID); err != nil {
		return err
	}
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
		// A fresh post-commit registry so the stale-blob cleanup registered by
		// saveFlowTx runs only after THIS transaction commits — without it,
		// RegisterPostCommit would fall back to running inline, deleting the
		// previous version's blob before the new version is durable.
		txCtx, postCommit := WithPostCommit(WithRLSTx(ctx, tx))
		if err := b.saveFlowTx(txCtx, flow, content); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("save flow: commit: %w", err)
		}
		committed = true
		postCommit.Run()
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
	//      unreferenced orphan, not corruption. (The next successful save reuses
	//      and overwrites that key; superseded blobs are deleted post-commit
	//      below, and DeleteFlow removes the whole prefix.)
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
		INSERT INTO flows (id, name, description, content, source, metadata, owner_id, org_id, created_at, updated_at, version)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6::jsonb, $7, $8, $9, $10, 0)
		ON CONFLICT (id) DO UPDATE SET
			name        = EXCLUDED.name,
			description = EXCLUDED.description,
			content     = EXCLUDED.content,
			source      = EXCLUDED.source,
			metadata    = EXCLUDED.metadata,
			updated_at  = EXCLUDED.updated_at,
			version     = flows.version + 1
		WHERE flows.version = $11
		RETURNING version`,
		flow.ID, flow.Name, flow.Description, string(dbContent), flow.Source, string(meta),
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
		// The previous version's content blob is now stale — without cleanup one
		// orphan accumulates per save for the flow's whole lifetime (and an
		// age-based storage lifecycle rule can't reclaim them: it cannot tell a
		// stale v(N-1) from the LIVE blob of a flow that simply hasn't been edited
		// lately). Delete it post-commit, after a grace delay that covers readers
		// who scanned the row at the old version just before this commit landed.
		// Registered only when a post-commit registry exists (RLS middleware or
		// the SaveFlow wrapper above): running it inline inside an uncommitted
		// transaction could delete the blob the row still points at if the commit
		// later fails. Without a registry the orphan is simply kept (safe).
		if newVersion > 0 && hasPostCommit(ctx) {
			flowID := flow.ID
			prevKey := flowContentKey(flowID, newVersion-1)
			RegisterPostCommit(ctx, func() {
				// notBefore is computed here (post-commit) so the grace window is
				// measured from when the new version actually became durable.
				b.scheduleBlobCleanup(time.Now().Add(staleBlobCleanupDelay), "stale-content:"+flowID, func(ctx context.Context) {
					b.deleteSingleBlob(ctx, flowID, prevKey)
				})
			})
		}
	}

	flow.UpdatedAt = now
	flow.Version = newVersion
	return nil
}

func (b *PostgresStorageBackend) TransferFlowOwner(ctx context.Context, flowID, newOwnerID, newOrgID string) error {
	// Bump version on transfer: SaveFlow's OCC contract keys off flows.version,
	// and owner_id/org_id are security-sensitive fields (the interface doc notes
	// this is the ONLY way to reassign ownership). Without a version bump a
	// client that read the flow before the transfer can still save afterward
	// (the WHERE flows.version = $11 check passes), silently reverting the new
	// ownership — and the version-keyed flow.changed broadcast wouldn't fire.
	res, err := b.query(ctx).ExecContext(ctx,
		`UPDATE flows SET owner_id = $1, org_id = $2, updated_at = $3, version = version + 1 WHERE id = $4`,
		newOwnerID, newOrgID, time.Now().UTC(), flowID)
	if err != nil {
		return fmt.Errorf("transfer flow owner: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// loadFlowRow reads the flow's DB row (no blob download). Content is the raw DB
// value — a "{}" placeholder when blob storage is configured.
func (b *PostgresStorageBackend) loadFlowRow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	row := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, name, description, content, source, metadata, owner_id, org_id, created_at, updated_at, version
		 FROM flows WHERE id = $1`, id)

	var flow interfaces.FlowDocument
	var contentRaw, metaRaw []byte
	if err := row.Scan(
		&flow.ID, &flow.Name, &flow.Description,
		&contentRaw, &flow.Source, &metaRaw,
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
		} else if metadataRecordsContent(flow.Metadata) {
			// Both keys 404 on a row whose metadata records content: the blob is
			// missing though it should exist — data loss, not a legitimately
			// empty flow. Fail rather than returning a hollow {}.
			metrics.RecordBlobContentMissing()
			return nil, fmt.Errorf("load flow %s: content blob missing though metadata records content (%d block(s))", flow.ID, flow.Metadata.BlockCount)
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

// ListFlows returns flows matching the filter: a SQL query/scan phase
// (queryFlowRows), followed by a concurrent blob-content backfill phase
// (backfillBlobContent) for any row whose full content wasn't inlined in the
// DB (only runs when the caller wants full content and blob storage is
// configured — UI listings use MetadataOnly and never reach it).
func (b *PostgresStorageBackend) ListFlows(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	result, err := b.queryFlowRows(ctx, filter)
	if err != nil {
		return nil, err
	}
	if !filter.MetadataOnly && b.blobClient != nil {
		if err := b.backfillBlobContent(ctx, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// queryFlowRows builds and runs the SELECT for ListFlows and scans the
// results. Content is omitted from the query (a cheap literal instead) when
// filter.MetadataOnly, so listing views never pay for the (potentially large)
// content JSONB.
func (b *PostgresStorageBackend) queryFlowRows(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	whereClause, args, n := flowFilterWhere(filter)

	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit, filter.Offset)

	// Avoid shipping the (potentially large) content JSONB + source text when
	// the caller only needs listing metadata — selecting literals keeps the row
	// shape identical to the full select.
	contentExpr := "content"
	sourceExpr := "source"
	if filter.MetadataOnly {
		contentExpr = "'{}'::jsonb AS content"
		sourceExpr = "'' AS source"
	}

	orderClause := flowOrderBy(filter.SortBy)
	q := fmt.Sprintf(`
		SELECT id, name, description, %s, %s, metadata, owner_id, org_id, created_at, updated_at, version
		FROM flows
		WHERE %s
		%s
		LIMIT $%d OFFSET $%d`,
		contentExpr, sourceExpr, whereClause, orderClause, n, n+1)

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
			&contentRaw, &flow.Source, &metaRaw,
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

	return result, rows.Err()
}

// backfillBlobContent concurrently downloads full content, from blob storage,
// for every flow in flows whose DB-stored content is empty or a placeholder.
// Mutates each *FlowDocument's Content in place.
func (b *PostgresStorageBackend) backfillBlobContent(ctx context.Context, flows []*interfaces.FlowDocument) error {
	g, gctx := errgroup.WithContext(ctx)
	// Bound concurrent Azure blob downloads: a page can hold up to 500 flows
	// (see the limit clamp in queryFlowRows), and firing that many downloads at
	// once risks Azure throttling (429s) and a memory spike holding every body
	// in flight. g.Go then blocks until a slot frees, so fetches still
	// parallelize. 16 matches the house cap in internal/ai (maxConcurrentRecords).
	g.SetLimit(16)
	for _, flow := range flows {
		if len(flow.Content) == 0 || string(flow.Content) == "{}" || string(flow.Content) == "null" {
			f := flow // capture range variable
			g.Go(func() (err error) {
				// errgroup does not recover panics raised in its goroutines, so
				// a panic in the azblob SDK would crash the process. Convert it
				// to an error so the list fails loudly like any other blob failure.
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("blob fetch for flow %s panicked: %v", f.ID, r)
					}
				}()
				// A transient blob failure fails the whole list. The only
				// full-content consumers are the governance scanner and the user
				// data export, and silently substituting placeholder content
				// harms both: the scanner would record bogus "healthy/empty"
				// analysis history (poisoning trends and alerts), and an export
				// must be complete or fail. UI listings use MetadataOnly and
				// never reach this path.
				content, err := b.downloadBlob(gctx, flowContentKey(f.ID, f.Version))
				if err != nil {
					return fmt.Errorf("list flows: download content for flow %s: %w", f.ID, err)
				}
				if content == nil {
					// Fall back to the legacy version-agnostic key.
					content, err = b.downloadBlob(gctx, legacyFlowContentKey(f.ID))
					if err != nil {
						return fmt.Errorf("list flows: download legacy content for flow %s: %w", f.ID, err)
					}
				}
				if content != nil {
					f.Content = content
					return nil
				}
				// Both keys 404. Clear the DB placeholder so callers can
				// distinguish "content unavailable" (nil) from real content:
				// the scanner then skips the flow instead of analyzing an
				// empty document. When metadata says content should exist this
				// is per-flow data loss — log it, but don't fail every caller
				// forever on one lost blob (the export layer re-checks and
				// fails loudly on its own).
				f.Content = nil
				if metadataRecordsContent(f.Metadata) {
					metrics.RecordBlobContentMissing()
					logger.Error("flow content blob missing though metadata records content", "flow_id", f.ID, "version", f.Version)
				}
				return nil
			})
		}
	}
	return g.Wait()
}

// DeleteFlow removes a flow by ID.
func (b *PostgresStorageBackend) DeleteFlow(ctx context.Context, id string) error {
	// Guard against an ID containing '/' before it is interpolated into the
	// blob-prefix below (validFlowID's docstring calls out this exact risk).
	// SaveFlow already validates; this covers delete/load paths that bypass it.
	if err := validFlowID(id); err != nil {
		return err
	}
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
			b.scheduleBlobCleanup(time.Now(), "delete-flow:"+id, func(ctx context.Context) {
				if err := b.deleteBlobs(ctx, prefix); err != nil {
					logger.Warn("failed to delete flow blobs", "flow_id", id, "error", err)
				}
			})
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

// blobOpTimeout bounds a single logical blob operation (all SDK retries
// included). The SDK's retry policy alone (TryTimeout 30s × up to 5 attempts
// plus backoff) could hold a request — and, in saveFlowTx, the flow row lock
// inside an open transaction — for minutes during a blob outage, piling up
// lock waiters until the connection pool drains. This cap keeps a blob outage
// a per-request failure instead of a pool-exhaustion incident.
const blobOpTimeout = 45 * time.Second

func (b *PostgresStorageBackend) uploadBlob(ctx context.Context, key string, data []byte) error {
	if b.blobClient == nil {
		return nil
	}
	if len(data) > maxBlobContentBytes {
		return fmt.Errorf("upload blob %s: content size %d bytes exceeds limit of %d bytes", key, len(data), maxBlobContentBytes)
	}
	ctx, cancel := context.WithTimeout(ctx, blobOpTimeout)
	defer cancel()
	start := time.Now()
	contentType := "application/json"
	_, err := b.blobClient.UploadBuffer(ctx, b.container, key, data, &azblob.UploadBufferOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType},
	})
	if err != nil {
		metrics.RecordBlobOp("upload", blobErrStatus(err), time.Since(start))
		return fmt.Errorf("upload blob %s: %w", key, err)
	}
	metrics.RecordBlobOp("upload", "ok", time.Since(start))
	return nil
}

// staleBlobCleanupDelay is the grace period before a superseded content blob is
// deleted. It covers readers that scanned the flow row at the previous version
// just before the new version committed and have not yet fetched the blob.
// Package-level so tests can shorten it.
var staleBlobCleanupDelay = time.Minute

// scheduleBlobCleanup routes a deferred blob-cleanup task through the bounded
// cleaner (created by New when blob storage is configured). If no cleaner is
// present — a backend constructed directly, or one already stopped — it falls
// back to a single detached goroutine so behaviour degrades to the previous
// best-effort model rather than dropping the work.
func (b *PostgresStorageBackend) scheduleBlobCleanup(notBefore time.Time, desc string, run func(ctx context.Context)) {
	if b.cleaner != nil && b.cleaner.enqueue(notBefore, desc, run) {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("blob cleanup goroutine panicked", "desc", desc, "err", r)
			}
		}()
		if d := time.Until(notBefore); d > 0 {
			time.Sleep(d)
		}
		ctx, cancel := context.WithTimeout(context.Background(), blobCleanupOpTimeout)
		defer cancel()
		run(ctx)
	}()
}

// deleteSingleBlob removes one blob, tolerating a 404 (the key may never have
// existed — fresh-insert races, pre-versioned-scheme flows — or a concurrent
// DeleteFlow already removed the whole prefix). ctx is supplied by the caller
// (the cleaner worker bounds it).
func (b *PostgresStorageBackend) deleteSingleBlob(ctx context.Context, flowID, key string) {
	start := time.Now()
	_, err := b.blobClient.DeleteBlob(ctx, b.container, key, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			metrics.RecordBlobOp("delete", "not_found", time.Since(start))
			return
		}
		metrics.RecordBlobOp("delete", blobErrStatus(err), time.Since(start))
		logger.Warn("failed to delete flow content blob", "flow_id", flowID, "key", key, "error", err)
		return
	}
	metrics.RecordBlobOp("delete", "ok", time.Since(start))
}

func (b *PostgresStorageBackend) downloadBlob(ctx context.Context, key string) ([]byte, error) {
	if b.blobClient == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, blobOpTimeout)
	defer cancel()
	start := time.Now()
	resp, err := b.blobClient.DownloadStream(ctx, b.container, key, &azblob.DownloadStreamOptions{})
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			metrics.RecordBlobOp("download", "not_found", time.Since(start))
			return nil, nil
		}
		// Defensively close an error response body if the SDK returned one
		// alongside the error (it generally closes them itself, but this is
		// robust against SDK version changes that could leak the connection).
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		metrics.RecordBlobOp("download", blobErrStatus(err), time.Since(start))
		return nil, fmt.Errorf("download blob %s: %w", key, err)
	}
	defer resp.Body.Close()
	// Cap the read at the same ceiling upload enforces: a blob larger than the
	// limit (written before the cap, by another tool, or after the constant was
	// lowered) must not be read unboundedly into memory — ListFlows downloads a
	// whole page of these concurrently. LimitReader takes limit+1 so an
	// exactly-limit blob still reads fully and only a truly oversized one trips.
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, maxBlobContentBytes+1)); err != nil {
		metrics.RecordBlobOp("download", "error", time.Since(start))
		return nil, fmt.Errorf("read blob %s: %w", key, err)
	}
	if buf.Len() > maxBlobContentBytes {
		metrics.RecordBlobOp("download", "error", time.Since(start))
		return nil, fmt.Errorf("download blob %s: content exceeds limit of %d bytes", key, maxBlobContentBytes)
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

// SaveFlowVersion persists a flow version snapshot. The version number is
// computed atomically (SELECT ... FOR UPDATE on the parent flow row + MAX+1)
// so concurrent saves for the same flow serialize and never collide. Only the
// most recent maxFlowVersionSnapshots snapshots are kept per flow; older ones
// are pruned (rows in the same transaction, blobs post-commit).
func (b *PostgresStorageBackend) SaveFlowVersion(ctx context.Context, v *interfaces.FlowVersion) error {
	if err := validFlowID(v.FlowID); err != nil {
		return err
	}
	if !hasRLSTx(ctx) {
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("save flow version: begin tx: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		// Fresh post-commit registry for the snapshot-prune blob cleanup — same
		// rationale as SaveFlow: it must run only after THIS commit.
		txCtx, postCommit := WithPostCommit(WithRLSTx(ctx, tx))
		if err := b.saveFlowVersionTx(txCtx, v); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("save flow version: commit: %w", err)
		}
		committed = true
		postCommit.Run()
		return nil
	}
	return b.saveFlowVersionTx(ctx, v)
}

// versionBlobKey is the content-blob storage key for a flow version, derived
// from the row id (unique per version) so it is stable and knowable BEFORE the
// version number is allocated — which lets SaveFlowVersion upload ahead of the
// parent flow's FOR UPDATE lock.
func versionBlobKey(flowID, versionID string) string {
	return fmt.Sprintf("flows/%s/versions/%s.json", flowID, versionID)
}

// versionBlobKeyLegacy is the pre-migration key derived from the version number.
// Used as a read/cleanup fallback for rows written before the blob_key column
// existed (blob_key == "").
func versionBlobKeyLegacy(flowID string, version int) string {
	return fmt.Sprintf("flows/%s/versions/%d/content.json", flowID, version)
}

func (b *PostgresStorageBackend) saveFlowVersionTx(ctx context.Context, v *interfaces.FlowVersion) error {
	q := b.query(ctx)

	meta, err := json.Marshal(v.Metadata)
	if err != nil {
		meta = []byte("{}")
	}
	content := v.Content
	if content == nil {
		content = json.RawMessage("{}")
	}

	// Upload the content blob BEFORE taking the parent flow's FOR UPDATE lock,
	// so the lock is never held across the (network) upload. The blob key is
	// derived from the row id (v.ID) rather than the version number, so it is
	// unique without needing the version — which lets the upload happen ahead of
	// the lock. Uploading before the INSERT also keeps the invariant that a row
	// never points at a missing blob.
	var blobKey string
	if b.blobClient != nil {
		blobKey = versionBlobKey(v.FlowID, v.ID)
		if err := b.uploadBlob(ctx, blobKey, content); err != nil {
			return err
		}
		content = json.RawMessage("{}")
	}

	// Lock the parent flow row to serialize concurrent version saves. Without
	// this, two callers can read the same max(version) and race on INSERT.
	var dummy int
	if err := q.QueryRowContext(ctx, `SELECT 1 FROM flows WHERE id = $1 FOR UPDATE`, v.FlowID).Scan(&dummy); err != nil {
		if blobKey != "" {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), blobOpTimeout)
			b.deleteSingleBlob(cleanupCtx, v.FlowID, blobKey)
			cancel()
		}
		return fmt.Errorf("lock flow for versioning: %w", err)
	}

	// Atomically compute the next version (overrides the caller's value).
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM flow_versions WHERE flow_id = $1`,
		v.FlowID,
	).Scan(&v.Version); err != nil {
		if blobKey != "" {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), blobOpTimeout)
			b.deleteSingleBlob(cleanupCtx, v.FlowID, blobKey)
			cancel()
		}
		return fmt.Errorf("compute next version: %w", err)
	}

	_, err = q.ExecContext(ctx,
		`INSERT INTO flow_versions (id, flow_id, version, comment, content, metadata, created_by, created_at, blob_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		v.ID, v.FlowID, v.Version, v.Comment, content, meta, v.CreatedBy, v.CreatedAt, blobKey,
	)
	if err != nil {
		// The INSERT failed (constraint, transient DB error, …): the row this
		// blob backs will never exist, so reclaim the just-uploaded blob now.
		// Without this the blob is orphaned forever — pruneFlowVersions only
		// schedules cleanup for rows it DELETEs, and this version's row was
		// never inserted. Best-effort: a delete failure here is logged, not
		// returned, since the original INSERT error is the actionable one. (A
		// later commit failure is the same accepted trade-off SaveFlow makes.)
		if blobKey != "" {
			// Best-effort reclaim on a bounded context so a hung blob delete
			// can't block the error return path. deleteSingleBlob logs its
			// own failures; the original INSERT error is the actionable one.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), blobOpTimeout)
			b.deleteSingleBlob(cleanupCtx, v.FlowID, blobKey)
			cancel()
		}
		return err
	}
	return b.pruneFlowVersions(ctx, q, v.FlowID)
}

// maxFlowVersionSnapshots caps the version history kept per flow. Snapshots
// (rows + their content blobs) previously grew unbounded: nothing pruned them
// until the flow itself was deleted. Package-level var so tests can lower it.
var maxFlowVersionSnapshots = 50

// pruneFlowVersions deletes snapshots beyond the retention cap (oldest first)
// and schedules their content blobs for post-commit deletion. Called under the
// parent flow's FOR UPDATE lock, so concurrent snapshot saves cannot race the
// prune. Blob cleanup is skipped when no post-commit registry is present —
// deleting a blob before the row delete is durable could destroy a snapshot
// that survives a rollback (the orphaned blob is the safe failure mode).
func (b *PostgresStorageBackend) pruneFlowVersions(ctx context.Context, q DBTX, flowID string) error {
	rows, err := q.QueryContext(ctx, `
		DELETE FROM flow_versions
		WHERE flow_id = $1 AND version NOT IN (
			SELECT version FROM flow_versions WHERE flow_id = $1
			ORDER BY version DESC LIMIT $2
		)
		RETURNING version, blob_key`, flowID, maxFlowVersionSnapshots)
	if err != nil {
		return fmt.Errorf("prune flow versions: %w", err)
	}
	defer rows.Close()
	var prunedKeys []string
	for rows.Next() {
		var ver int
		var blobKey string
		if err := rows.Scan(&ver, &blobKey); err != nil {
			return fmt.Errorf("scan pruned version: %w", err)
		}
		// Prefer the stored key; fall back to the legacy version-derived key for
		// rows written before the blob_key column existed.
		if blobKey == "" {
			blobKey = versionBlobKeyLegacy(flowID, ver)
		}
		prunedKeys = append(prunedKeys, blobKey)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("prune flow versions: %w", err)
	}
	if b.blobClient != nil && len(prunedKeys) > 0 && hasPostCommit(ctx) {
		keys := prunedKeys
		RegisterPostCommit(ctx, func() {
			// Although the pruned rows are gone after commit, an in-flight reader
			// may have already SELECTed the row (under its own snapshot) and be
			// about to download the blob. Use the same reader-grace delay as the
			// stale-content path so such a reader doesn't hit a 404.
			for _, key := range keys {
				k := key
				b.scheduleBlobCleanup(time.Now().Add(staleBlobCleanupDelay), "prune-version:"+flowID, func(ctx context.Context) {
					b.deleteSingleBlob(ctx, flowID, k)
				})
			}
		})
	}
	return nil
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
				logger.Warn("failed to parse flow version metadata JSON", "error", err, "flow_id", v.FlowID, "version", v.Version)
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
	var blobKey string
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, flow_id, version, comment, content, metadata, created_by, created_at, blob_key
		 FROM flow_versions WHERE flow_id = $1 AND version = $2`,
		flowID, version,
	).Scan(&v.ID, &v.FlowID, &v.Version, &v.Comment, &v.Content, &metaRaw, &v.CreatedBy, &v.CreatedAt, &blobKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load flow version: %w", err)
	}
	if len(metaRaw) > 0 {
		if err := json.Unmarshal(metaRaw, &v.Metadata); err != nil {
			logger.Warn("failed to parse flow version metadata JSON", "error", err, "flow_id", v.FlowID, "version", v.Version)
		}
	}

	// Download from Blob Storage if configured and DB content is placeholder.
	if b.blobClient != nil && (len(v.Content) == 0 || string(v.Content) == "{}" || string(v.Content) == "null") {
		// Prefer the stored key; fall back to the legacy version-derived key for
		// rows written before the blob_key column existed.
		if blobKey == "" {
			blobKey = versionBlobKeyLegacy(v.FlowID, v.Version)
		}
		content, err := b.downloadBlob(ctx, blobKey)
		if err != nil {
			return nil, fmt.Errorf("load flow version %s/%d: content unavailable in blob storage: %w", v.FlowID, v.Version, err)
		}
		if content != nil {
			v.Content = content
		} else if metadataRecordsContent(v.Metadata) {
			metrics.RecordBlobContentMissing()
			return nil, fmt.Errorf("load flow version %s/%d: content blob missing though metadata records content (%d block(s))", v.FlowID, v.Version, v.Metadata.BlockCount)
		}
	}

	return v, nil
}
