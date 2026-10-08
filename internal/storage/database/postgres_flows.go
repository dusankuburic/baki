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
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// maxBlobContentBytes bounds a single flow-content blob: the payload is held in memory on upload and download.
const maxBlobContentBytes = 50 << 20

// flowContentKey is the version-keyed blob key for a flow's current content: a racing or rolled-back
// write leaves an unreferenced orphan instead of clobbering live content; the OCC space never collides with "versions/N".
func flowContentKey(flowID string, version int) string {
	return fmt.Sprintf("flows/%s/content.v%d.json", flowID, version)
}

// legacyFlowContentKey is the original version-agnostic key; LoadFlow falls back to it for flows written before versioned keys.
func legacyFlowContentKey(flowID string) string {
	return fmt.Sprintf("flows/%s/content.json", flowID)
}

func blobErrStatus(err error) string {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) && respErr.StatusCode == 429 {
		return "throttled"
	}
	return "error"
}

// validFlowID rejects IDs containing '/': such an ID would nest one flow's blobs inside another's prefix and let its owner delete or shadow that flow's content.
func validFlowID(id string) error {
	if id == "" {
		return errors.New("flow id must not be empty")
	}
	if strings.Contains(id, "/") {
		return fmt.Errorf("invalid flow id %q: must not contain '/'", id)
	}
	return nil
}

// metadataRecordsContent reports whether a missing content blob would be data loss rather than a legitimately empty flow.
func metadataRecordsContent(m interfaces.FlowMetadata) bool {
	return m.BlockCount > 0 || m.SubflowCount > 0 || m.FileSize > 0 || m.RawLineCount > 0
}

// SaveFlow upserts a flow document: existing rows update only when flow.Version matches (OCC —
// stale returns ErrVersionConflict); new rows insert at version 0 regardless of flow.Version.
func (b *PostgresStorageBackend) SaveFlow(ctx context.Context, flow *interfaces.FlowDocument) error {
	now := time.Now().UTC()
	return b.writeFlow(ctx, flow, now, now, flow.Version)
}

// importNoVersion is the stored version ImportFlow expects: no row has it, so saveFlowSQL inserts or writes nothing.
const importNoVersion = -1

// ImportFlow inserts a flow that is not stored yet at version 0 with its own created and updated times (zero: now,
// then the created time), for the legacy migration. A stored flow is left alone and reported as ErrVersionConflict.
func (b *PostgresStorageBackend) ImportFlow(ctx context.Context, flow *interfaces.FlowDocument) error {
	created, updated := flow.CreatedAt.UTC(), flow.UpdatedAt.UTC()
	if flow.CreatedAt.IsZero() {
		created = time.Now().UTC()
	}
	if flow.UpdatedAt.IsZero() {
		updated = created
	}
	if err := b.writeFlow(ctx, flow, created, updated, importNoVersion); err != nil {
		return err
	}
	flow.CreatedAt = created
	return nil
}

// writeFlow is SaveFlow with the new row's times and the stored version the write expects given.
func (b *PostgresStorageBackend) writeFlow(ctx context.Context, flow *interfaces.FlowDocument, created, updated time.Time, expectedVer int) error {
	if err := validFlowID(flow.ID); err != nil {
		return err
	}
	content := flow.Content
	if len(content) == 0 {
		content = []byte("{}")
	}

	// The version bump and the content upload must commit together or the row could point at a
	// version whose blob never landed: withTx joins the request's transaction, or opens one so a failed
	// upload rolls the bump back. Its post-commit registry runs the stale-blob cleanup only after the new
	// version is durable.
	if b.blobClient != nil {
		return b.withTx(ctx, func(txCtx context.Context, _ *sql.Tx) error {
			return b.saveFlowTx(txCtx, flow, content, created, updated, expectedVer)
		})
	}
	return b.saveFlowTx(ctx, flow, content, created, updated, expectedVer)
}

// saveFlowSQL upserts the flows row AND its flow_search side row in ONE statement (a failed OCC
// check yields no `up` row → ErrVersionConflict); TOASTed columns rewrite only when changed (IS
// DISTINCT FROM); the result row comes from `up` since the search INSERT may legitimately write nothing.
const saveFlowSQL = `
		WITH up AS (
			INSERT INTO flows (id, name, description, content, source, metadata, owner_id, org_id, created_at, updated_at, version)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6::jsonb, $7, $8, $9, $10, 0)
			ON CONFLICT (id) DO UPDATE SET
				name        = EXCLUDED.name,
				description = EXCLUDED.description,
				content     = CASE WHEN flows.content IS DISTINCT FROM EXCLUDED.content THEN EXCLUDED.content ELSE flows.content END,
				source      = CASE WHEN flows.source  IS DISTINCT FROM EXCLUDED.source  THEN EXCLUDED.source  ELSE flows.source  END,
				metadata    = EXCLUDED.metadata,
				updated_at  = EXCLUDED.updated_at,
				version     = flows.version + 1
			WHERE flows.version = $11
			RETURNING id, name, content, version
		), fs AS (
			INSERT INTO flow_search (flow_id, name, body)
			SELECT up.id, up.name, COALESCE(NULLIF(up.content->>'source', ''), up.content::text) FROM up
			ON CONFLICT (flow_id) DO UPDATE SET name = EXCLUDED.name, body = EXCLUDED.body
			WHERE flow_search.name IS DISTINCT FROM EXCLUDED.name OR flow_search.body IS DISTINCT FROM EXCLUDED.body
		)
		SELECT version FROM up`

// saveFlowTx performs the conditional upsert and optional blob upload via b.query(ctx); content is the normalized content.
// created stamps an inserted row only, updated every written row.
func (b *PostgresStorageBackend) saveFlowTx(ctx context.Context, flow *interfaces.FlowDocument, content []byte, created, updated time.Time, expectedVer int) error {
	// In blob mode the DB row holds only a "{}" placeholder: the blob is written only after the OCC
	// check succeeds (a rejected writer never touches it) and keyed by the *new* DB version, so a
	// failed later COMMIT leaves the row on the previous (correct) blob, the new one an orphan.
	dbContent := content
	if b.blobClient != nil {
		dbContent = []byte("{}")
	}

	meta, err := json.Marshal(flow.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	var newVersion int
	err = b.query(ctx).QueryRowContext(ctx, saveFlowSQL,
		flow.ID, flow.Name, flow.Description, string(dbContent), flow.Source, string(meta),
		flow.OwnerID, flow.OrganizationID, created, updated, expectedVer,
	).Scan(&newVersion)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return interfaces.ErrVersionConflict
		}
		return fmt.Errorf("upsert flow: %w", err)
	}

	// OCC check passed — only now persist the real content, under the new version's key so a
	// rejected concurrent write can never clobber stored content.
	if b.blobClient != nil {
		if err := b.uploadBlob(ctx, flowContentKey(flow.ID, newVersion), content); err != nil {
			return err
		}
		// The previous version's blob is now stale (an age-based lifecycle rule can't tell it from the
		// LIVE blob of a rarely-edited flow): delete it post-commit, after a grace delay for readers
		// that scanned the old version — inline inside an uncommitted tx it could delete the blob the row still points at.
		if newVersion > 0 && txhook.Has(ctx) {
			flowID := flow.ID
			prevKey := flowContentKey(flowID, newVersion-1)
			txhook.Register(ctx, func(context.Context) {
				// notBefore computed post-commit so the grace window starts when the new version became durable.
				b.scheduleBlobCleanup(time.Now().Add(staleBlobCleanupDelay), "stale-content:"+flowID, func(ctx context.Context) {
					b.deleteSingleBlob(ctx, flowID, prevKey)
				})
			})
		}
	}

	flow.UpdatedAt = updated
	flow.Version = newVersion
	return nil
}

// loadFlowRow reads the flow's DB row without the blob; content is the raw DB value ("{}" placeholder in blob mode).
func (b *PostgresStorageBackend) loadFlowRow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	row := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, name, description, content, source, metadata, owner_id, org_id, created_at, updated_at, version, tags
		 FROM flows WHERE id = $1`, id)

	var flow interfaces.FlowDocument
	var contentRaw, metaRaw []byte
	var tagsCSV string
	if err := row.Scan(
		&flow.ID, &flow.Name, &flow.Description,
		&contentRaw, &flow.Source, &metaRaw,
		&flow.OwnerID, &flow.OrganizationID,
		&flow.CreatedAt, &flow.UpdatedAt, &flow.Version,
		&tagsCSV,
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
	flow.Tags = interfaces.SplitFlowTags(tagsCSV)
	return &flow, nil
}

// flowHeaderQuery selects the flow row WITHOUT content/source: both are flow-sized TOASTed columns,
// and every permission/ResolveDoc-cache/OCC-version read goes through LoadFlowHeader — never select them here.
const flowHeaderQuery = `SELECT id, name, description, metadata, owner_id, org_id, created_at, updated_at, version, tags
	 FROM flows WHERE id = $1`

// LoadFlowHeader retrieves a flow's metadata WITHOUT content or source, so authorize/existence/version
// callers don't pay for the document and don't fail when blob storage is briefly unavailable.
func (b *PostgresStorageBackend) LoadFlowHeader(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	var flow interfaces.FlowDocument
	var metaRaw []byte
	var tagsCSV string
	if err := b.query(ctx).QueryRowContext(ctx, flowHeaderQuery, id).Scan(
		&flow.ID, &flow.Name, &flow.Description, &metaRaw,
		&flow.OwnerID, &flow.OrganizationID,
		&flow.CreatedAt, &flow.UpdatedAt, &flow.Version,
		&tagsCSV,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("scan flow header: %w", err)
	}
	if err := json.Unmarshal(metaRaw, &flow.Metadata); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	flow.Tags = interfaces.SplitFlowTags(tagsCSV)
	return &flow, nil
}

// LoadFlow retrieves a flow document by ID, including its content (from blob storage when configured).
func (b *PostgresStorageBackend) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	flow, err := b.loadFlowRow(ctx, id)
	if err != nil {
		return nil, err
	}

	if b.blobClient != nil && (len(flow.Content) == 0 || string(flow.Content) == "{}" || string(flow.Content) == "null") {
		content, err := b.downloadBlob(ctx, flowContentKey(flow.ID, flow.Version))
		if err != nil {
			// Unreachable blob storage: a hollow flow would silently corrupt the caller's view — fail loudly.
			return nil, fmt.Errorf("load flow %s: content unavailable in blob storage: %w", flow.ID, err)
		}
		if content == nil {
			content, err = b.downloadBlob(ctx, legacyFlowContentKey(flow.ID))
			if err != nil {
				return nil, fmt.Errorf("load flow %s: content unavailable in blob storage: %w", flow.ID, err)
			}
		}
		if content != nil {
			flow.Content = content
		} else if metadataRecordsContent(flow.Metadata) {
			// Both keys 404 on metadata-recorded content: data loss, not a legitimately empty flow —
			// fail rather than return a hollow {}.
			metrics.RecordBlobContentMissing()
			return nil, fmt.Errorf("load flow %s: content blob missing though metadata records content (%d block(s))", flow.ID, flow.Metadata.BlockCount)
		}
	}

	return flow, nil
}

// flowFilterWhere builds the WHERE clause and args for a FlowFilter; shared by ListFlows and CountFlows so they never drift.
func flowFilterWhere(filter interfaces.FlowFilter) (string, sqlArgs) {
	where := []string{"1=1"}
	args := sqlArgs{}

	// The visibility set is `id IN (arm UNION ALL arm …)` — one index-backed arm per access path
	// rather than an OR with a correlated EXISTS, which forces a sequential scan of flows for every
	// library page and count; IN returns a multi-path-visible flow once. Lists bind as `= ANY($n)`
	// so the statement shape never varies (pgx statement cache keys on text).
	switch {
	case filter.AllFlows:
		// Explicit operational enumeration (migration): no owner/org scoping.
	case filter.SharedOnly:
		// "Shared with me" — collaborator rows only, excluding owned flows.
		if filter.UserID == "" {
			return "1=0", nil
		}
		user := args.add(filter.UserID)
		where = append(where, "owner_id <> "+user+" AND id IN (SELECT flow_id FROM flow_collaborators WHERE user_id = "+user+")")
	case filter.UserID == "" && filter.OrganizationID == "" && len(filter.OrganizationIDs) == 0:
		// Defense-in-depth: unscoped would match every row — always-false instead, so a caller
		// who forgets to set filter fields cannot dump all data.
		return "1=0", nil
	case filter.UserID != "":
		user := args.add(filter.UserID)
		arms := []string{"SELECT id FROM flows WHERE owner_id = " + user}
		if filter.OrganizationID != "" {
			arms = append(arms, "SELECT id FROM flows WHERE org_id = "+args.add(filter.OrganizationID))
		}
		if len(filter.OrganizationIDs) > 0 {
			arms = append(arms, "SELECT id FROM flows WHERE org_id = ANY("+args.add(textArray(filter.OrganizationIDs))+")")
		}
		arms = append(arms, "SELECT flow_id FROM flow_collaborators WHERE user_id = "+user)
		where = append(where, "id IN ("+strings.Join(arms, " UNION ALL ")+")")
	case filter.OrganizationID != "":
		where = append(where, "org_id = "+args.add(filter.OrganizationID))
	case len(filter.OrganizationIDs) > 0:
		where = append(where, "org_id = ANY("+args.add(textArray(filter.OrganizationIDs))+")")
	}

	if filter.Query != "" {
		// Name substring via flow_search (v27): its trigram index is usable, the one on flows never
		// was (FORCE RLS + non-leakproof operator); flow_search has no RLS — tenant-correct but a weak cross-tenant timing oracle.
		where = append(where, "id IN (SELECT flow_id FROM flow_search WHERE name ILIKE "+args.add("%"+likeLiteral(filter.Query)+"%")+")")
	}
	if filter.CreatedAfter != nil {
		where = append(where, "created_at > "+args.add(*filter.CreatedAfter))
	}
	if filter.CreatedBefore != nil {
		where = append(where, "created_at < "+args.add(*filter.CreatedBefore))
	}
	// Keyset cursor for id-ordered walks: the next page starts after the last id seen instead of OFFSET re-reading.
	if filter.AfterID != "" {
		where = append(where, "id > "+args.add(filter.AfterID))
	}

	// Tags: ANY-of semantics; delimiter-anchored LIKE over the CSV so 'prod' never matches 'production'.
	if len(filter.Tags) > 0 {
		normalized, err := interfaces.NormalizeFlowTags(filter.Tags)
		if err != nil || len(normalized) == 0 {
			return "1=0", nil // invalid tag in a filter matches nothing
		}
		// One array bind so the statement shape does not vary with the number of tags. Each tag is a LIKE pattern,
		// escaped so that "_" (a valid tag character) is not a wildcard.
		patterns := make([]string, len(normalized))
		for i, tag := range normalized {
			patterns[i] = likeLiteral(tag)
		}
		where = append(where, "EXISTS (SELECT 1 FROM unnest("+args.add(textArray(patterns))+
			"::text[]) AS t(tag) WHERE (',' || tags || ',') LIKE ('%,' || t.tag || ',%'))")
	}

	return strings.Join(where, " AND "), args
}

// textArray copies ids into a fresh slice for `= ANY($n)` binds, keeping the caller's slice from the driver.
func textArray(ids []string) []string {
	out := make([]string, len(ids))
	copy(out, ids)
	return out
}

// countFlowsSQL is the CountFlows statement for filter (exposed for the plan tests).
func countFlowsSQL(filter interfaces.FlowFilter) (string, []any) {
	whereClause, args := flowFilterWhere(filter)
	return "SELECT count(*) FROM flows WHERE " + whereClause, args
}

// listFlowsSQL is the ListFlows statement for filter with its args (exposed for the plan tests).
func listFlowsSQL(filter interfaces.FlowFilter) (string, []any) {
	return listFlowsSQLFor(filter, "")
}

// listFlowsWithTotalSQL is the ListFlowsWithTotal statement (plan tests): list + COUNT(*) OVER()
// riding every row, so the total returns with the rows instead of a second visibility semi-join + RLS evaluation.
func listFlowsWithTotalSQL(filter interfaces.FlowFilter) (string, []any) {
	return listFlowsSQLFor(filter, ", COUNT(*) OVER() AS page_total")
}

func listFlowsSQLFor(filter interfaces.FlowFilter, extraSelect string) (string, []any) {
	whereClause, args := flowFilterWhere(filter)

	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	// Select literals instead of content/source when MetadataOnly, keeping the row shape identical.
	contentExpr := "content"
	sourceExpr := "source"
	if filter.MetadataOnly {
		contentExpr = "'{}'::jsonb AS content"
		sourceExpr = "'' AS source"
	}

	orderClause := flowOrderBy(filter.SortBy)
	q := fmt.Sprintf(`
		SELECT id, name, description, %s, %s, metadata, owner_id, org_id, created_at, updated_at, version, tags%s
		FROM flows
		WHERE %s
		%s
		LIMIT %s OFFSET %s`,
		contentExpr, sourceExpr, extraSelect, whereClause, orderClause, args.add(limit), args.add(filter.Offset))
	return q, args
}

// flowOrderBy whitelists ORDER BY columns, never interpolating user input. Every clause ends in
// `id` as a unique tiebreaker — whole-table LIMIT/OFFSET walkers silently skip or duplicate tied
// rows without it (ties are ordinary: updated_at is microsecond-resolution, bulk writes tie).
func flowOrderBy(s interfaces.FlowSort) string {
	switch s {
	case interfaces.FlowSortUpdatedAsc:
		return "ORDER BY updated_at ASC, id ASC"
	case interfaces.FlowSortNameAsc:
		return "ORDER BY lower(name) ASC, updated_at DESC, id ASC"
	case interfaces.FlowSortNameDesc:
		return "ORDER BY lower(name) DESC, updated_at DESC, id ASC"
	case interfaces.FlowSortBlocksDesc:
		return "ORDER BY COALESCE((metadata->>'BlockCount')::int, 0) DESC, updated_at DESC, id ASC"
	case interfaces.FlowSortIDAsc:
		// Immutable sort key (see FlowSortIDAsc); already unique, no tiebreaker needed.
		return "ORDER BY id ASC"
	default:
		return "ORDER BY updated_at DESC, id ASC"
	}
}

// CountFlows returns the total matching the filter, ignoring Limit/Offset.
func (b *PostgresStorageBackend) CountFlows(ctx context.Context, filter interfaces.FlowFilter) (int, error) {
	q, args := countFlowsSQL(filter)
	var total int
	err := b.query(ctx).QueryRowContext(ctx, q, args...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count flows: %w", err)
	}
	return total, nil
}

// ListFlows returns matching flows: a query/scan phase, then a concurrent blob-content backfill
// for rows whose content wasn't inlined (only full-content callers with blob storage configured; UI listings use MetadataOnly).
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

// ListFlowsWithTotal is ListFlows plus the unpaginated total in ONE statement: the visibility
// semi-join and flows RLS policy are the read path's most expensive predicates. The total rides the
// rows, so an empty page past the first is counted on its own (CountFlows).
func (b *PostgresStorageBackend) ListFlowsWithTotal(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, int, error) {
	docs, total, err := b.queryFlowRowsTotal(ctx, filter, true)
	if err != nil {
		return nil, 0, err
	}
	if len(docs) == 0 && filter.Offset > 0 {
		if total, err = b.CountFlows(ctx, filter); err != nil {
			return nil, 0, err
		}
	}
	if !filter.MetadataOnly && b.blobClient != nil {
		if err := b.backfillBlobContent(ctx, docs); err != nil {
			return nil, 0, err
		}
	}
	return docs, total, nil
}

// queryFlowRows builds and runs the ListFlows SELECT and scans the results (literal content when MetadataOnly).
func (b *PostgresStorageBackend) queryFlowRows(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	docs, _, err := b.queryFlowRowsTotal(ctx, filter, false)
	return docs, err
}

// queryFlowRowsTotal runs the list SELECT, optionally with COUNT(*) OVER() riding each row.
func (b *PostgresStorageBackend) queryFlowRowsTotal(ctx context.Context, filter interfaces.FlowFilter, withTotal bool) ([]*interfaces.FlowDocument, int, error) {
	var q string
	var args []any
	if withTotal {
		q, args = listFlowsWithTotalSQL(filter)
	} else {
		q, args = listFlowsSQL(filter)
	}
	rows, err := b.query(ctx).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list flows: %w", err)
	}
	defer rows.Close()
	return scanFlowList(rows, filter.MetadataOnly, withTotal)
}

// flowListColumns are a flow listing's columns in the order scanFlowList reads them (listFlowsSQLFor spells them out
// to substitute content and source for MetadataOnly).
const flowListColumns = `id, name, description, content, source, metadata, owner_id, org_id, created_at, updated_at, version, tags`

// scanFlowList reads the rows of a flow listing (flowListColumns, then the page total when withTotal).
func scanFlowList(rows *sql.Rows, metadataOnly, withTotal bool) ([]*interfaces.FlowDocument, int, error) {
	var result []*interfaces.FlowDocument
	var pageTotal int
	for rows.Next() {
		var flow interfaces.FlowDocument
		var contentRaw, metaRaw []byte
		var tagsCSV string
		dests := []any{
			&flow.ID, &flow.Name, &flow.Description,
			&contentRaw, &flow.Source, &metaRaw,
			&flow.OwnerID, &flow.OrganizationID,
			&flow.CreatedAt, &flow.UpdatedAt, &flow.Version,
			&tagsCSV,
		}
		if withTotal {
			dests = append(dests, &pageTotal)
		}
		if err := rows.Scan(dests...); err != nil {
			return nil, 0, fmt.Errorf("scan row: %w", err)
		}
		flow.Content = contentRaw
		if metadataOnly {
			// Keep Content nil (not "{}") so "MetadataOnly ⇒ empty Content" holds across backends.
			flow.Content = nil
		}
		if err := json.Unmarshal(metaRaw, &flow.Metadata); err != nil {
			return nil, 0, fmt.Errorf("unmarshal metadata: %w", err)
		}
		flow.Tags = interfaces.SplitFlowTags(tagsCSV)
		result = append(result, &flow)
	}

	return result, pageTotal, rows.Err()
}

// backfillBlobContent concurrently downloads full content from blob storage for every flow whose
// DB-stored content is a placeholder, mutating each *FlowDocument's Content in place.
func (b *PostgresStorageBackend) backfillBlobContent(ctx context.Context, flows []*interfaces.FlowDocument) error {
	g, gctx := errgroup.WithContext(ctx)
	// Bound concurrent downloads: 500-flow pages risk Azure throttling (429s) and a memory spike;
	// g.Go blocks until a slot frees, so fetches still parallelize.
	g.SetLimit(16)
	for _, flow := range flows {
		if len(flow.Content) == 0 || string(flow.Content) == "{}" || string(flow.Content) == "null" {
			f := flow // capture range variable
			g.Go(func() (err error) {
				// errgroup does not recover goroutine panics — convert one to an error so the list fails loudly.
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("blob fetch for flow %s panicked: %v", f.ID, r)
					}
				}()
				// A transient blob failure fails the whole list: the only full-content consumers
				// (governance scanner, user data export) are both harmed by placeholder substitution.
				content, err := b.downloadBlob(gctx, flowContentKey(f.ID, f.Version))
				if err != nil {
					return fmt.Errorf("list flows: download content for flow %s: %w", f.ID, err)
				}
				if content == nil {
					content, err = b.downloadBlob(gctx, legacyFlowContentKey(f.ID))
					if err != nil {
						return fmt.Errorf("list flows: download legacy content for flow %s: %w", f.ID, err)
					}
				}
				if content != nil {
					f.Content = content
					return nil
				}
				// Both keys 404: clear the placeholder so callers distinguish "content unavailable" (nil)
				// from real content; per-flow data loss is logged, not a permanent error for every caller.
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
	// Guard '/' before it is interpolated into the blob prefix below (SaveFlow validates; this covers delete/load).
	if err := validFlowID(id); err != nil {
		return err
	}
	// conversations.flow_id deliberately has no FK to flows (chats about never-persisted flows must
	// survive), so delete them here BEFORE the flows row: the conversations RLS policy resolves
	// visibility by joining flows, so removing the flow first would hide these rows from this statement.
	if _, err := b.query(ctx).ExecContext(ctx, `DELETE FROM conversations WHERE flow_id = $1`, id); err != nil {
		return fmt.Errorf("delete flow conversations: %w", err)
	}
	res, err := b.query(ctx).ExecContext(ctx, `DELETE FROM flows WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete flow: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return interfaces.ErrNotFound
	}

	// Delete the flow's blobs post-commit: deleting while the surrounding RLS tx is still open
	// would orphan a surviving row's content if the request rolls back.
	if b.blobClient != nil {
		prefix := fmt.Sprintf("flows/%s/", id)
		txhook.Register(ctx, func(context.Context) {
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
	// IS DISTINCT FROM skips the write for byte-identical history: messages is a wide TOASTed
	// column, and an unchanged history also keeps its updated_at.
	_, err = b.query(ctx).ExecContext(ctx, `
		INSERT INTO conversations (flow_id, scope, messages, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (flow_id, scope) DO UPDATE SET
			messages   = EXCLUDED.messages,
			updated_at = EXCLUDED.updated_at
		WHERE conversations.messages IS DISTINCT FROM EXCLUDED.messages`,
		flowID, scope, data, time.Now().UTC())
	return err
}

// LoadConversation retrieves the conversation for a flow+scope; a non-nil empty slice when none
// exists (filesystem parity — nil always indicates an error).
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

// DeleteConversation removes the conversation for a flow+scope; missing is a no-op (idempotent "clear history").
func (b *PostgresStorageBackend) DeleteConversation(ctx context.Context, flowID, scope string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM conversations WHERE flow_id = $1 AND scope = $2`, flowID, scope)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	return nil
}

// ListCollaborators returns the collaborators for a flow. Every collaborator statement reaches the grants through
// their flow: flow_collaborators cannot carry a row-level policy (every flow policy reads it, so one would recurse),
// but flows can, so inside a user's RLS scope a flow the caller cannot see has no grants to read or write (a write
// answers ErrNotFound). Without a scope every flow is reachable.
func (b *PostgresStorageBackend) ListCollaborators(ctx context.Context, flowID string, limit int) ([]*interfaces.Collaborator, error) {
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT c.user_id, u.email, c.permission, c.granted_at
		FROM flow_collaborators c
		JOIN users u ON c.user_id = u.id
		WHERE c.flow_id = $1
		  AND EXISTS (SELECT 1 FROM flows f WHERE f.id = c.flow_id)
		ORDER BY c.granted_at ASC
		LIMIT CASE WHEN $2 > 0 THEN $2 ELSE NULL END`, flowID, limit)
	if err != nil {
		return nil, fmt.Errorf("query collaborators: %w", err)
	}
	defer rows.Close()

	// Contract: empty result is non-nil (filesystem parity); nil is reserved for errors.
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

// ListCollaboratorsBatch returns collaborators for multiple flows in one query; map keys are flow
// IDs, flows with no collaborators are absent (treat missing keys as empty).
func (b *PostgresStorageBackend) ListCollaboratorsBatch(ctx context.Context, flowIDs []string) (map[string][]*interfaces.Collaborator, error) {
	result := make(map[string][]*interfaces.Collaborator, len(flowIDs))
	if len(flowIDs) == 0 {
		return result, nil
	}
	seen := make(map[string]bool, len(flowIDs))
	ids := make([]string, 0, len(flowIDs))
	for _, id := range flowIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return result, nil
	}
	// One array bind: the statement text is the same for any page size.
	const q = `SELECT c.flow_id, c.user_id, u.email, c.permission, c.granted_at
		FROM flow_collaborators c
		JOIN users u ON c.user_id = u.id
		WHERE c.flow_id = ANY($1)
		  AND EXISTS (SELECT 1 FROM flows f WHERE f.id = c.flow_id)
		ORDER BY c.granted_at ASC`
	rows, err := b.query(ctx).QueryContext(ctx, q, ids)
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

// blobOpTimeout bounds one logical blob op (SDK retries included): unbounded retries would hold
// the flow row lock — and pile up lock waiters until the pool drains — during a blob outage; the
// cap keeps the outage a per-request failure instead of a pool-exhaustion incident.
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

// staleBlobCleanupDelay is the grace period before a superseded content blob is deleted, covering
// readers that scanned the previous version just before commit; package-level so tests can shorten it.
var staleBlobCleanupDelay = time.Minute

// scheduleBlobCleanup routes a deferred blob-cleanup task through the bounded cleaner, falling
// back to a single detached goroutine so the work is never dropped.
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

// deleteSingleBlob removes one blob, tolerating a 404 (never existed, or a concurrent prefix delete).
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
		// Defensively close an error body if the SDK returned one (robust against SDK connection leaks).
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		metrics.RecordBlobOp("download", blobErrStatus(err), time.Since(start))
		return nil, fmt.Errorf("download blob %s: %w", key, err)
	}
	defer resp.Body.Close()
	// Cap the read at the upload ceiling — ListFlows downloads a whole page of these concurrently;
	// LimitReader takes limit+1 so an exactly-limit blob reads fully and only a truly oversized one trips.
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

	// flush deletes the accumulated blob names in one Blob Batch (up to 256 per round trip).
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
		// The batch call can succeed as a whole while sub-requests fail (e.g. concurrently deleted
		// blobs) — count but don't abort, so leaks stay observable.
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
	// Surface partial failures so leaked blobs stay observable.
	if failed > 0 {
		return fmt.Errorf("delete blobs under %q: %d blob(s) failed to delete", prefix, failed)
	}
	return nil
}

func (b *PostgresStorageBackend) AddCollaborator(ctx context.Context, flowID string, c *interfaces.Collaborator) error {
	if c.GrantedAt.IsZero() {
		c.GrantedAt = time.Now().UTC()
	}
	res, err := b.query(ctx).ExecContext(ctx, `
		INSERT INTO flow_collaborators (flow_id, user_id, permission, granted_at)
		SELECT f.id, $2, $3, $4 FROM flows f WHERE f.id = $1
		ON CONFLICT (flow_id, user_id) DO UPDATE SET
			permission = EXCLUDED.permission,
			granted_at = EXCLUDED.granted_at`,
		flowID, c.UserID, c.Permission, c.GrantedAt)
	if err != nil {
		return fmt.Errorf("add collaborator: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

func (b *PostgresStorageBackend) UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error {
	res, err := b.query(ctx).ExecContext(ctx, `
		UPDATE flow_collaborators SET permission = $1
		WHERE flow_id = $2 AND user_id = $3
		  AND EXISTS (SELECT 1 FROM flows f WHERE f.id = $2)`,
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
		WHERE flow_id = $1 AND user_id = $2
		  AND EXISTS (SELECT 1 FROM flows f WHERE f.id = $1)`,
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

// SaveFlowVersion persists a flow version snapshot: the version is computed atomically (FOR UPDATE
// on the parent row + MAX+1) so concurrent saves serialize; only the newest maxFlowVersionSnapshots
// are kept (rows pruned in-tx, blobs post-commit).
func (b *PostgresStorageBackend) SaveFlowVersion(ctx context.Context, v *interfaces.FlowVersion) error {
	if err := validFlowID(v.FlowID); err != nil {
		return err
	}
	// The prune's blob cleanup must run only after the new snapshot commits (same rationale as SaveFlow).
	return b.withTx(ctx, func(txCtx context.Context, _ *sql.Tx) error {
		return b.saveFlowVersionTx(txCtx, v)
	})
}

// versionBlobKey derives the snapshot blob key from the row id, so it is knowable BEFORE the
// version number is allocated — letting SaveFlowVersion upload ahead of the parent's FOR UPDATE lock.
func versionBlobKey(flowID, versionID string) string {
	return fmt.Sprintf("flows/%s/versions/%s.json", flowID, versionID)
}

// versionBlobKeyLegacy is the pre-migration version-number-derived key; read/cleanup fallback for rows with blob_key == "".
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

	// Upload BEFORE taking the parent's FOR UPDATE lock so the lock is never held across the
	// (network) upload; the row-id-derived key needs no version, and a row never points at a missing blob.
	var blobKey string
	if b.blobClient != nil {
		blobKey = versionBlobKey(v.FlowID, v.ID)
		if err := b.uploadBlob(ctx, blobKey, content); err != nil {
			return err
		}
		content = json.RawMessage("{}")
	}

	// Lock the parent row to serialize concurrent version saves (else two callers read the same MAX and race the INSERT).
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
		// INSERT failed: the row this blob backs will never exist — reclaim the just-uploaded blob
		// now (pruneFlowVersions only schedules cleanup for rows it DELETEs); best-effort.
		if blobKey != "" {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), blobOpTimeout)
			b.deleteSingleBlob(cleanupCtx, v.FlowID, blobKey)
			cancel()
		}
		return err
	}
	return b.pruneFlowVersions(ctx, q, v.FlowID)
}

// maxFlowVersionSnapshots caps the version history kept per flow; a var so tests can lower it.
var maxFlowVersionSnapshots = 50

// pruneFlowVersions deletes snapshots beyond the retention cap (oldest first) under the parent's
// FOR UPDATE lock, so prunes cannot race concurrent saves. Blob cleanup runs post-commit only —
// earlier could destroy a snapshot that survives a rollback.
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
		// Fall back to the legacy version-derived key for rows predating the blob_key column.
		if blobKey == "" {
			blobKey = versionBlobKeyLegacy(flowID, ver)
		}
		prunedKeys = append(prunedKeys, blobKey)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("prune flow versions: %w", err)
	}
	if b.blobClient != nil && len(prunedKeys) > 0 && txhook.Has(ctx) {
		keys := prunedKeys
		txhook.Register(ctx, func(context.Context) {
			// Same reader-grace delay as the stale-content path: an in-flight reader may hold a pruned row's snapshot.
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

	// Download from blob storage when the DB content is a placeholder.
	if b.blobClient != nil && (len(v.Content) == 0 || string(v.Content) == "{}" || string(v.Content) == "null") {
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

// ── Flow tags ────────────────────────────────────────────────────────

// UpdateFlowTags replaces a flow's tags WITHOUT a content round-trip or version bump — re-tagging must not trip OCC.
func (b *PostgresStorageBackend) UpdateFlowTags(ctx context.Context, flowID string, tags []string) error {
	normalized, err := interfaces.NormalizeFlowTags(tags)
	if err != nil {
		return err
	}
	res, err := b.query(ctx).ExecContext(ctx,
		`UPDATE flows SET tags = $2, updated_at = NOW() WHERE id = $1`,
		flowID, strings.Join(normalized, ","))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// likeLiteral escapes LIKE wildcards so needle matches as a literal substring.
func likeLiteral(needle string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(needle)
}

// searchFlowContentsSQL is the SearchFlowContents statement (exposed for the plan tests); needle is
// trimmed and non-empty, limit clamped.
func searchFlowContentsSQL(filter interfaces.FlowFilter, needle string, limit int) (string, []any) {
	where, args := flowFilterWhere(filter)
	// The match runs on flow_search (v27): its trigram indexes are usable by the app role, the one on
	// flows(content::text) never was (FORCE RLS + non-leakproof operators). body-only for parity with
	// the old predicate — SearchLibrary re-ranks the capped candidate list in process, so name-only
	// hits would displace content hits beyond the cap. flow_search has no RLS: tenant-correct, but a
	// weak cross-tenant timing oracle (pre-filtering by the caller's visible ids cannot use the index).
	pattern := "%" + likeLiteral(needle) + "%"
	q := fmt.Sprintf(`
		SELECT id, name, description, '{}'::jsonb AS content, '' AS source,
		       metadata, owner_id, org_id, created_at, updated_at, version, tags
		FROM flows
		WHERE %s AND id IN (SELECT flow_id FROM flow_search WHERE body ILIKE %s)
		ORDER BY updated_at DESC
		LIMIT %s`,
		where, args.add(pattern), args.add(limit))
	return q, args
}

// SearchFlowContents pushes library content search into the database: ILIKE over flow_search.body
// (v27) under the caller's visibility filter, newest first, metadata-only; blob-offloaded deployments
// return ErrContentSearchUnsupported for the legacy scan.
func (b *PostgresStorageBackend) SearchFlowContents(ctx context.Context, filter interfaces.FlowFilter, needle string, limit int) ([]*interfaces.FlowDocument, error) {
	if b.blobClient != nil {
		return nil, interfaces.ErrContentSearchUnsupported
	}
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return []*interfaces.FlowDocument{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q, args := searchFlowContentsSQL(filter, needle, limit)
	rows, err := b.query(ctx).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("content search: %w", err)
	}
	defer rows.Close()

	var out []*interfaces.FlowDocument
	for rows.Next() {
		var f interfaces.FlowDocument
		var contentRaw, metaRaw []byte
		var tagsCSV string
		if err := rows.Scan(
			&f.ID, &f.Name, &f.Description,
			&contentRaw, &f.Source, &metaRaw,
			&f.OwnerID, &f.OrganizationID,
			&f.CreatedAt, &f.UpdatedAt, &f.Version,
			&tagsCSV,
		); err != nil {
			return nil, err
		}
		f.Tags = interfaces.SplitFlowTags(tagsCSV)
		out = append(out, &f)
	}
	return out, rows.Err()
}
