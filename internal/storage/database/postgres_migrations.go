package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
)

// migrationChecksum returns the hex-encoded SHA-256 of a migration's SQL. It is
// recorded when the step is applied and re-verified on every subsequent boot so
// that editing an already-shipped migration (which would silently diverge the
// schema across deployments) is caught instead of ignored.
func migrationChecksum(sqlText string) string {
	sum := sha256.Sum256([]byte(sqlText))
	return hex.EncodeToString(sum[:])
}

// ---- Schema migration ----

// migrationLockKey is an arbitrary fixed 64-bit integer used as the key for
// pg_advisory_lock during schema migration. Any value works as long as it is
// unique to this application — pick something derived from the project name
// so it does not collide with locks taken by other software sharing the DB.
const migrationLockKey int64 = 0x70616461_6E616C7A // "padanalz" in hex bytes

// migration is a single forward-only schema step. The baseline is the entire
// original idempotent schema; subsequent steps are appended in order. Each step
// is applied at most once (recorded in schema_migrations) under the advisory
// lock, so two pods starting concurrently can't both apply the same step.
//
// There is intentionally no automatic down/rollback path — DDL changes are
// forward-compatible by discipline (CREATE/ALTER ... IF NOT EXISTS, additive
// columns with defaults). A bad change requires manual SQL to reverse, so keep
// new steps additive and small.
type migration struct {
	version int
	name    string
	sql     string
	// downSQL is the optional reverse of sql. Empty ("") means the step is
	// intentionally irreversible (the baseline, or a step whose inverse is not
	// safely expressible) — MigrateDown refuses to cross such a step. The
	// down-migration is NEVER checksummed (only forward sql is) and NEVER runs
	// on boot; it is operator-invoked via bakicli.
	downSQL string
}

// migrations is the ordered, append-only set of schema versions. Version 1 is
// the baseline: the entire original idempotent schema, recorded so existing
// deployments stop re-running it every boot and so future steps build on a
// known version. Append new versions only at the end; never edit or reorder an
// already-shipped step.
var migrations = []migration{
	{version: 1, name: "baseline", sql: schemaBaseline, downSQL: ""}, // intentionally irreversible
	{version: 2, name: "comments_and_shares", sql: commentsAndSharesSQL, downSQL: commentsAndSharesDownSQL},
	{version: 3, name: "refresh_token_device_info", sql: refreshSessionDdlSQL, downSQL: refreshSessionDdlDownSQL},
	{version: 4, name: "flow_analysis_v2", sql: flowAnalysisV2SQL, downSQL: flowAnalysisV2DownSQL},
	{version: 5, name: "finding_status_created_at", sql: findingStatusCreatedAtSQL, downSQL: findingStatusCreatedAtDownSQL},
	{version: 6, name: "perf_indexes", sql: perfIndexesSQL, downSQL: perfIndexesDownSQL},
	{version: 7, name: "usage_metrics_org_index", sql: usageMetricsOrgIndexSQL, downSQL: usageMetricsOrgIndexDownSQL},
	{version: 8, name: "policies", sql: policiesSQL, downSQL: policiesDownSQL},
	{version: 9, name: "flow_blockcount_sort_index", sql: flowBlockCountIndexSQL, downSQL: flowBlockCountIndexDownSQL},
	{version: 10, name: "flow_versions_blob_key", sql: flowVersionsBlobKeySQL, downSQL: flowVersionsBlobKeyDownSQL},
	{version: 11, name: "pgvector_knowledge", sql: pgvectorKnowledgeSQL, downSQL: pgvectorKnowledgeDownSQL},
	{version: 12, name: "governance_alerts", sql: governanceAlertsSQL, downSQL: governanceAlertsDownSQL},
	{version: 13, name: "flows_name_trgm_index", sql: flowsNameTrgmSQL, downSQL: flowsNameTrgmDownSQL},
	{version: 14, name: "api_token_scopes", sql: apiTokenScopesSQL, downSQL: apiTokenScopesDownSQL},
	{version: 15, name: "flow_tags", sql: flowTagsSQL, downSQL: flowTagsDownSQL},
	{version: 16, name: "gov_alert_targets", sql: govAlertTargetsSQL, downSQL: govAlertTargetsDownSQL},
	{version: 17, name: "org_channels", sql: orgChannelsSQL, downSQL: orgChannelsDownSQL},
	{version: 18, name: "flows_content_trgm_index", sql: flowsContentTrgmSQL, downSQL: flowsContentTrgmDownSQL},
}

// flowBlockCountIndexSQL adds an expression index matching the FlowSortBlocksDesc
// ORDER BY (`COALESCE((metadata->>'BlockCount')::int, 0) DESC, updated_at DESC`),
// which previously forced a full-table scan + in-memory sort on that sort mode.
// The cast is safe: metadata.BlockCount is always a JSON number (or the key is
// absent → NULL → 0), so the immutable expression never throws at index build.
const flowBlockCountIndexSQL = `
CREATE INDEX IF NOT EXISTS flows_blockcount_updated_idx
    ON flows ((COALESCE((metadata->>'BlockCount')::int, 0)) DESC, updated_at DESC);
`

// flowVersionsBlobKeySQL records the content blob's storage key per version row.
// Previously the key was derived from (flow_id, version), which forced the blob
// upload to happen while holding the parent flow's FOR UPDATE lock (the version
// number isn't known until then). Storing an explicit key lets SaveFlowVersion
// key the blob on the row id and upload BEFORE taking the lock. Additive with an
// empty-string default; legacy rows keep "" and fall back to the derived key.
const flowVersionsBlobKeySQL = `
ALTER TABLE flow_versions ADD COLUMN IF NOT EXISTS blob_key TEXT NOT NULL DEFAULT '';
`

// flowVersionsBlobKeyDownSQL reverses v10. Data-lossy: the stored blob key is
// discarded; legacy rows that relied on the derived key keep working (the read
// path falls back), but rows written post-v10 with a non-legacy key would need
// re-derivation. Idempotent.
const flowVersionsBlobKeyDownSQL = `
ALTER TABLE flow_versions DROP COLUMN IF EXISTS blob_key;
`

// flowBlockCountIndexDownSQL reverses v9 (drop the expression index).
const flowBlockCountIndexDownSQL = `
DROP INDEX IF EXISTS flows_blockcount_updated_idx;
`

// policiesDownSQL reverses v8. Dropping the table also drops its index + RLS
// policies. Data-lossy: saved governance policies are destroyed.
const policiesDownSQL = `
DROP TABLE IF EXISTS policies;
`

// usageMetricsOrgIndexDownSQL reverses v7: drop the composite, restore the
// single-column org_id index that the forward step removed.
const usageMetricsOrgIndexDownSQL = `
DROP INDEX IF EXISTS usage_metrics_org_created_idx;
CREATE INDEX IF NOT EXISTS usage_metrics_org_id_idx ON usage_metrics (org_id);
`

// perfIndexesDownSQL reverses v6: drop the three added composite indexes and
// restore the six redundant single-column indexes the forward step removed.
const perfIndexesDownSQL = `
DROP INDEX IF EXISTS audit_events_user_created_idx;
DROP INDEX IF EXISTS refresh_tokens_expires_at_idx;
DROP INDEX IF EXISTS token_blacklist_expires_at_idx;
CREATE INDEX IF NOT EXISTS audit_events_user_id_idx ON audit_events (user_id);
CREATE INDEX IF NOT EXISTS flows_owner_id_idx ON flows (owner_id);
CREATE INDEX IF NOT EXISTS flows_org_id_idx ON flows (org_id);
CREATE INDEX IF NOT EXISTS usage_metrics_user_id_idx ON usage_metrics (user_id);
CREATE INDEX IF NOT EXISTS flow_versions_flow_id_idx ON flow_versions (flow_id);
CREATE INDEX IF NOT EXISTS idx_share_tokens_hash ON share_tokens (token_hash);
`

// findingStatusCreatedAtDownSQL reverses v5. Data-lossy: the MTTR created_at
// column (and its data) is dropped.
const findingStatusCreatedAtDownSQL = `
ALTER TABLE finding_status DROP COLUMN IF EXISTS created_at;
`

// flowAnalysisV2DownSQL reverses v4: drop the six dashboard-rollup columns
// added to flow_analysis and flow_analysis_history. Data-lossy.
const flowAnalysisV2DownSQL = `
ALTER TABLE flow_analysis DROP COLUMN IF EXISTS by_confidence;
ALTER TABLE flow_analysis DROP COLUMN IF EXISTS auto_fixable_count;
ALTER TABLE flow_analysis DROP COLUMN IF EXISTS total_findings;
ALTER TABLE flow_analysis_history DROP COLUMN IF EXISTS by_confidence;
ALTER TABLE flow_analysis_history DROP COLUMN IF EXISTS auto_fixable_count;
ALTER TABLE flow_analysis_history DROP COLUMN IF EXISTS total_findings;
`

// refreshSessionDdlDownSQL reverses v3: drop the device-info columns added to
// refresh_tokens. Data-lossy (UA/IP session labels destroyed).
const refreshSessionDdlDownSQL = `
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS user_agent;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS ip;
`

// commentsAndSharesDownSQL reverses v2: drop the share_tokens and
// finding_comments tables (their RLS policies drop with the tables). Order
// matters only relative to FKs to flows, which both share — IF EXISTS makes
// either order safe. Data-lossy: comments + share links destroyed.
const commentsAndSharesDownSQL = `
DROP TABLE IF EXISTS share_tokens;
DROP TABLE IF EXISTS finding_comments;
`

// pgvectorKnowledgeSQL enables server-side similarity search over knowledge-base
// embeddings. The column is intentionally DIMENSIONLESS (`vector`, pgvector
// 0.7+) so the schema doesn't need to be rebuilt when the embedding provider
// changes dimension; instead the application enforces the configured dimension
// at insert time (only same-dimension chunks populate embedding_vec, the rest
// stay NULL and are excluded from the index search, mirroring the "re-index
// after changing the embedding provider" contract).
//
// Everything is guarded on the extension being installable: a deployment whose
// Postgres role can't CREATE EXTENSION vector (or where the extension isn't
// packaged) must still boot. The column/index/backfill then no-op, and
// SearchKnowledge falls back to the in-Go cosine ranker. The backfill converts
// the existing JSONB embedding array (`[0.1,0.2,…]`) into the vector text form
// pgvector accepts; rows that fail (e.g. a corrupt JSONB array) are left NULL.
const pgvectorKnowledgeSQL = `
-- Best-effort extension install. Swallowed if the role lacks privilege or the
-- extension isn't packaged — the DO block below only proceeds when it lands.
DO $$ BEGIN
	CREATE EXTENSION IF NOT EXISTS vector;
EXCEPTION WHEN OTHERS THEN
	RAISE NOTICE 'pgvector extension unavailable; knowledge search stays Go-side';
END $$;

DO $$ BEGIN
	IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
		ALTER TABLE knowledge_chunks ADD COLUMN IF NOT EXISTS embedding_vec vector;

		-- Backfill existing rows from the JSONB embedding array. Only touches rows
		-- still NULL so the step is idempotent across re-runs.
		UPDATE knowledge_chunks
		SET embedding_vec = ('[' ||
			(SELECT string_agg(e, ',') FROM jsonb_array_elements_text(embedding) AS e)
			|| ']')::vector
		WHERE embedding_vec IS NULL
		  AND jsonb_typeof(embedding) = 'array';

		-- HNSW over IVFFlat: no training step, better for incremental inserts.
		CREATE INDEX IF NOT EXISTS knowledge_chunks_embedding_hnsw
			ON knowledge_chunks USING hnsw (embedding_vec vector_cosine_ops);
	END IF;
EXCEPTION WHEN OTHERS THEN
	RAISE NOTICE 'pgvector knowledge setup skipped: %', SQLERRM;
END $$;
`

// pgvectorKnowledgeDownSQL is the reverse of v11: drop the embedding_vec column
// (its HNSW index drops automatically with the column) but do NOT DROP the
// vector EXTENSION — it is database-wide and may serve other uses. The JSONB
// embedding column remains, so SearchKnowledge falls back to the Go-side cosine
// ranker. Idempotent. Guarded so a down without the extension/column is a no-op.
const pgvectorKnowledgeDownSQL = `
DO $$ BEGIN
	IF EXISTS (SELECT 1 FROM information_schema.columns
	           WHERE table_name = 'knowledge_chunks' AND column_name = 'embedding_vec') THEN
		ALTER TABLE knowledge_chunks DROP COLUMN embedding_vec;
	END IF;
EXCEPTION WHEN OTHERS THEN
	RAISE NOTICE 'pgvector knowledge down skipped: %', SQLERRM;
END $$;
`

// findingStatusCreatedAtSQL adds a created_at column to finding_status so the
// dashboard can compute mean-time-to-resolve (MTTR) and stale-finding counts.
// Additive with a default — existing rows backfill to NOW() (migration time),
// which the MTTR query excludes via `updated_at >= created_at`, so only findings
// triaged through a full lifecycle post-migration contribute to the average.
const findingStatusCreatedAtSQL = `
ALTER TABLE finding_status ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
`

// perfIndexesSQL adds composite indexes that eliminate full-table scans on hot
// paths, and drops redundant indexes whose maintenance is pure write/storage
// overhead on the hottest tables. All statements are idempotent (IF EXISTS).
//
// Added:
//   - audit_events(user_id, created_at DESC) — ListAuditEvents and the dashboard
//     activity feed filter by user_id and ORDER BY created_at DESC. Without this
//     composite, Postgres range-scans the single-column user_id index then does
//     an in-memory sort.
//   - refresh_tokens(expires_at) — the login/refresh hot path runs
//     DELETE FROM refresh_tokens WHERE expires_at < NOW() on every issuance.
//     Without an index this is a full table scan.
//   - token_blacklist(expires_at) — a per-process cleanup goroutine runs
//     DELETE FROM token_blacklist WHERE expires_at < NOW() every 5 minutes.
//     Without an index this is a full table scan.
//
// Dropped (each is covered by a composite or UNIQUE constraint's leftmost prefix):
//   - audit_events_user_id_idx  — covered by the new composite above
//   - flows_owner_id_idx        — covered by flows_owner_updated_idx(owner_id, updated_at DESC)
//   - flows_org_id_idx          — covered by flows_org_updated_idx(org_id, updated_at DESC)
//   - usage_metrics_user_id_idx — covered by usage_metrics_user_created_idx(user_id, created_at)
//   - flow_versions_flow_id_idx — covered by UNIQUE(flow_id, version)
//   - idx_share_tokens_hash     — covered by UNIQUE(token_hash)
const perfIndexesSQL = `
-- Add composite indexes for hot-path queries.
CREATE INDEX IF NOT EXISTS audit_events_user_created_idx ON audit_events (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS refresh_tokens_expires_at_idx ON refresh_tokens (expires_at);
CREATE INDEX IF NOT EXISTS token_blacklist_expires_at_idx ON token_blacklist (expires_at);

-- Drop redundant indexes (write/storage overhead on the hottest tables).
DROP INDEX IF EXISTS audit_events_user_id_idx;
DROP INDEX IF EXISTS flows_owner_id_idx;
DROP INDEX IF EXISTS flows_org_id_idx;
DROP INDEX IF EXISTS usage_metrics_user_id_idx;
DROP INDEX IF EXISTS flow_versions_flow_id_idx;
DROP INDEX IF EXISTS idx_share_tokens_hash;
`

// usageMetricsOrgIndexSQL adds a composite (org_id, created_at) index on
// usage_metrics. The AI budget-check hot path (GetDailyUsage) filters by
// org_id AND created_at on every AI call, but previously only had a
// single-column org_id index, forcing a range scan + in-memory date filter.
// The single-column index is dropped (covered by the composite's leftmost prefix).
const usageMetricsOrgIndexSQL = `
CREATE INDEX IF NOT EXISTS usage_metrics_org_created_idx ON usage_metrics (org_id, created_at);
DROP INDEX IF EXISTS usage_metrics_org_id_idx;
`

// flowAnalysisV2SQL adds the dashboard-rollup columns to flow_analysis (and
// its append-only history mirror) so the welcome dashboard can compute
// org-wide "confidence distribution" and "fix availability" KPIs without
// re-analyzing every flow. Additive columns with defaults — safe on existing
// rows, which backfill to {} / 0 and are refreshed on each flow's next analyze.
const flowAnalysisV2SQL = `
ALTER TABLE flow_analysis ADD COLUMN IF NOT EXISTS by_confidence JSONB NOT NULL DEFAULT '{}';
ALTER TABLE flow_analysis ADD COLUMN IF NOT EXISTS auto_fixable_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE flow_analysis ADD COLUMN IF NOT EXISTS total_findings INTEGER NOT NULL DEFAULT 0;

ALTER TABLE flow_analysis_history ADD COLUMN IF NOT EXISTS by_confidence JSONB NOT NULL DEFAULT '{}';
ALTER TABLE flow_analysis_history ADD COLUMN IF NOT EXISTS auto_fixable_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE flow_analysis_history ADD COLUMN IF NOT EXISTS total_findings INTEGER NOT NULL DEFAULT 0;
`

// refreshSessionDdlSQL records the User-Agent and client IP a refresh
// token was issued to, so the "active sessions" UI can show a friendly device
// label ("Firefox on Windows") and a rough location hint instead of just a
// timestamp. Additive columns with defaults — safe on existing rows.
const refreshSessionDdlSQL = `
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS ip TEXT NOT NULL DEFAULT '';
`

const commentsAndSharesSQL = `
CREATE TABLE IF NOT EXISTS finding_comments (
    id TEXT PRIMARY KEY,
    flow_id TEXT NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    finding_key TEXT NOT NULL,
    author_id TEXT NOT NULL,
    author_name TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_finding_comments_flow_key ON finding_comments(flow_id, finding_key);
ALTER TABLE finding_comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE finding_comments FORCE ROW LEVEL SECURITY;
CREATE POLICY finding_comments_modify ON finding_comments
    USING (
        NOT app_rls_active()
        OR EXISTS (
            SELECT 1 FROM flows f
            WHERE f.id = finding_comments.flow_id
              AND (f.owner_id = app_current_user_id()
                   OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
                   OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
        )
    ) WITH CHECK (
        NOT app_rls_active()
        OR EXISTS (
            SELECT 1 FROM flows f
            WHERE f.id = finding_comments.flow_id
              AND (f.owner_id = app_current_user_id()
                   OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
                   OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
        )
    );

-- share_tokens deliberately has NO row-level security: the public share
-- viewer (/api/shared) must look up tokens with no authenticated user in the
-- RLS session var. Every authenticated query against this table therefore
-- relies entirely on handler-level authz (flow "editor" via GetAuthorized)
-- — do not add share_tokens queries without such a gate.
CREATE TABLE IF NOT EXISTS share_tokens (
    id TEXT PRIMARY KEY,
    flow_id TEXT NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    created_by TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_share_tokens_flow ON share_tokens(flow_id);
CREATE INDEX IF NOT EXISTS idx_share_tokens_hash ON share_tokens(token_hash);
`

// migrate applies every pending migration, serializing concurrent startups
// across replicas using a PostgreSQL session-level advisory lock. Without the
// lock, two pods starting simultaneously can both execute a migration block and
// one will fail with a duplicate-object or constraint-violation error.
//
// Each step runs in its own transaction together with its version record, so a
// failure rolls both back and the step is retried on the next boot. The lock is
// held for the whole run; pg_advisory_lock is released when the session ends,
// so we dedicate a single connection to the migration and release the lock
// explicitly before returning it to the pool.
func (b *PostgresStorageBackend) migrate(ctx context.Context) error {
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		// Best-effort release; if it fails the lock will free when the conn closes.
		_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	if err := ensureMigrationsTable(ctx, conn); err != nil {
		return fmt.Errorf("ensure schema_migrations table: %w", err)
	}

	current, err := currentVersion(ctx, conn)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	// Integrity gate: verify that every already-applied migration's SQL still
	// matches the checksum recorded when it was applied. A mismatch means a
	// shipped migration was edited in place — fail boot rather than run new
	// steps on top of a divergent schema.
	if err := verifyChecksums(ctx, conn, current); err != nil {
		return err
	}

	applied := 0
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, conn, m); err != nil {
			return fmt.Errorf("apply migration v%d %q: %w", m.version, m.name, err)
		}
		slog.Info("schema migration applied", "version", m.version, "name", m.name, "from", current)
		current = m.version
		applied++
	}

	latest := migrations[len(migrations)-1].version
	if applied == 0 {
		slog.Info("schema up to date", "version", latest)
	} else {
		slog.Info("schema migrations complete", "version", latest)
	}
	return nil
}

// ensureMigrationsTable creates the version-tracking table if it does not yet
// exist. Existing deployments that predate versioning get it created on first
// boot; their already-applied baseline is then recorded as v1 by applyMigration
// (the baseline SQL is fully idempotent, so re-running it is a no-op).
func ensureMigrationsTable(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER    PRIMARY KEY,
		name       TEXT       NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return err
	}
	// checksum records the SHA-256 of the step's SQL. Added additively so
	// deployments that predate checksumming get the column on first boot; their
	// existing rows default to '' ("unknown") and are backfilled by
	// verifyChecksums rather than treated as drift.
	_, err := conn.ExecContext(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT NOT NULL DEFAULT ''`)
	return err
}

// verifyChecksums compares each already-applied migration's recorded checksum
// against the current SQL's checksum. Rows recorded before checksumming existed
// (empty checksum) are adopted by backfilling the current value — we cannot know
// the original SQL, so we trust the running binary once. A non-empty mismatch is
// a fatal drift error.
func verifyChecksums(ctx context.Context, conn *sql.Conn, current int) error {
	stored, err := loadAppliedChecksums(ctx, conn)
	if err != nil {
		return fmt.Errorf("load migration checksums: %w", err)
	}
	for _, m := range migrations {
		if m.version > current {
			continue // not applied yet — checksum written at apply time
		}
		got, ok := stored[m.version]
		if !ok {
			continue // recorded elsewhere / gap; nothing to compare
		}
		want := migrationChecksum(m.sql)
		if got == "" {
			// Pre-checksum deployment: adopt the current checksum as baseline.
			if _, err := conn.ExecContext(ctx,
				`UPDATE schema_migrations SET checksum = $1 WHERE version = $2`, want, m.version); err != nil {
				return fmt.Errorf("backfill checksum v%d: %w", m.version, err)
			}
			slog.Info("schema migration checksum backfilled", "version", m.version, "name", m.name)
			continue
		}
		if got != want {
			return fmt.Errorf(
				"schema migration drift detected: v%d %q checksum mismatch (recorded=%s computed=%s); "+
					"an already-applied migration's SQL was modified — never edit a shipped migration, append a new one",
				m.version, m.name, got, want)
		}
	}
	return nil
}

// loadAppliedChecksums returns version → recorded checksum for every row in
// schema_migrations.
func loadAppliedChecksums(ctx context.Context, conn *sql.Conn) (map[int]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]string)
	for rows.Next() {
		var v int
		var c sql.NullString
		if err := rows.Scan(&v, &c); err != nil {
			return nil, err
		}
		out[v] = c.String
	}
	return out, rows.Err()
}

func currentVersion(ctx context.Context, conn *sql.Conn) (int, error) {
	var v int
	err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// applyMigration runs one step's SQL and records its version in a single
// transaction, so a half-applied step can never be marked complete.
func applyMigration(ctx context.Context, conn *sql.Conn, m migration) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("exec sql: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.version, m.name, migrationChecksum(m.sql)); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	return tx.Commit()
}

// applyMigrationDown runs one step's downSQL and removes its version record in
// a single transaction, mirroring applyMigration so a half-rolled-back step can
// never be left in a partial state. The downSQL is NOT checksummed (only the
// forward sql is immutable); deleting the version row makes verifyChecksums
// naturally skip it on the next boot.
func applyMigrationDown(ctx context.Context, conn *sql.Conn, m migration) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, m.downSQL); err != nil {
		return fmt.Errorf("exec down sql: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = $1`, m.version); err != nil {
		return fmt.Errorf("delete version record: %w", err)
	}
	return tx.Commit()
}

// MigrationStep describes one migration for the operator-facing down tooling.
type MigrationStep struct {
	Version int
	Name    string
	// Reversible is false when downSQL is empty (the step is intentionally
	// irreversible and MigrateDown refuses to cross it).
	Reversible bool
}

// MigrationSteps exposes the ordered step list + reversibility for bakicli's
// `migrate down` planning/help. It is the complete set the binary can apply.
func MigrationSteps() []MigrationStep {
	out := make([]MigrationStep, 0, len(migrations))
	for _, m := range migrations {
		out = append(out, MigrationStep{Version: m.version, Name: m.name, Reversible: m.downSQL != ""})
	}
	return out
}

// ErrMigrationNotReversible is returned by MigrateDown when the rollback path
// crosses a step with no downSQL (e.g. the baseline). The operator must restore
// from backup instead.
var ErrMigrationNotReversible = fmt.Errorf("migration not reversible: a step in the rollback path has no down-migration (baseline or unsafe); restore from backup instead")

// MigrateDown rolls the schema back to targetVersion by running each applied
// step's downSQL in strict descending order, each in its own transaction under
// the migration advisory lock. It is the operator-invoked reverse of migrate()
// and is NEVER run on boot. A step with empty downSQL makes the whole rollback
// fail with ErrMigrationNotReversible — the caller (bakicli) is expected to
// pre-check the path and refuse interactively before any destructive work.
//
// Returns the list of step versions that were rolled back (descending), which
// the caller surfaces to the operator.
func (b *PostgresStorageBackend) MigrateDown(ctx context.Context, targetVersion int) ([]int, error) {
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return nil, fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	current, err := currentVersion(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	if targetVersion >= current {
		return nil, nil // nothing to do (already at or below target)
	}

	// Pre-check the entire rollback path BEFORE any destructive work: every
	// step from current down to targetVersion+1 must have a downSQL. This keeps
	// a "not reversible" failure atomic — no half-rolled-back schema.
	for v := current; v > targetVersion; v-- {
		step, ok := migrationByVersion(v)
		if !ok {
			return nil, fmt.Errorf("unknown migration version %d in rollback path", v)
		}
		if step.downSQL == "" {
			return nil, fmt.Errorf("%w: v%d %q", ErrMigrationNotReversible, step.version, step.name)
		}
	}

	var rolledBack []int
	for v := current; v > targetVersion; v-- {
		step := mustMigrationByVersion(v) // presence guaranteed by the pre-check
		slog.Warn("schema migration rolling back", "version", step.version, "name", step.name)
		if err := applyMigrationDown(ctx, conn, step); err != nil {
			return rolledBack, fmt.Errorf("apply down-migration v%d %q: %w", step.version, step.name, err)
		}
		rolledBack = append(rolledBack, step.version)
		current = step.version - 1
	}
	slog.Warn("schema rollback complete", "to_version", current)
	return rolledBack, nil
}

func migrationByVersion(v int) (migration, bool) {
	for _, m := range migrations {
		if m.version == v {
			return m, true
		}
	}
	return migration{}, false
}

func mustMigrationByVersion(v int) migration {
	m, ok := migrationByVersion(v)
	if !ok {
		panic(fmt.Sprintf("migration version %d not found (pre-check should have caught this)", v))
	}
	return m
}

// CurrentSchemaVersion reports the highest applied migration version, or 0 when
// no backend/migrations table exists (local mode). Exposed for health/observability.
func (b *PostgresStorageBackend) CurrentSchemaVersion(ctx context.Context) (int, error) {
	if b == nil || b.db == nil {
		return 0, nil
	}
	var v int
	err := b.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return v, nil
}

const schemaBaseline = `
CREATE TABLE IF NOT EXISTS users (
	id          TEXT        PRIMARY KEY,
	email       TEXT        UNIQUE NOT NULL,
	password    TEXT        NOT NULL,
	role        TEXT        NOT NULL,
	email_verified BOOLEAN    NOT NULL DEFAULT FALSE,
	failed_login_attempts INTEGER NOT NULL DEFAULT 0,
	locked_until TIMESTAMPTZ,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Profile fields (added post-1.0; ALTER for existing deployments).
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS failed_login_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS locked_until TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_url TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS flows (
	id          TEXT        PRIMARY KEY,
	name        TEXT        NOT NULL,
	description TEXT        NOT NULL DEFAULT '',
	content     JSONB       NOT NULL DEFAULT '{}',
	source      TEXT        NOT NULL DEFAULT '',
	metadata    JSONB       NOT NULL DEFAULT '{}',
	owner_id    TEXT        NOT NULL DEFAULT '',
	org_id      TEXT        NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	version     INTEGER     NOT NULL DEFAULT 0
);
-- OCC version column (added post-1.0; ALTER for existing deployments).
ALTER TABLE flows ADD COLUMN IF NOT EXISTS version INTEGER NOT NULL DEFAULT 0;
-- Raw PAD source text (added for cloud-mode apply-fix/preview-fix, which patch
-- line-based source rather than the parsed content). Empty for legacy rows.
ALTER TABLE flows ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS flows_owner_id_idx ON flows (owner_id);
CREATE INDEX IF NOT EXISTS flows_org_id_idx   ON flows (org_id);
-- Supports ORDER BY updated_at DESC on every list query (avoids full-scan sort).
CREATE INDEX IF NOT EXISTS flows_updated_at_idx ON flows (updated_at DESC);
-- Supports the per-row collaborator EXISTS subquery in ListFlows and ListCollaborators.
CREATE INDEX IF NOT EXISTS users_role_idx ON users (role);

CREATE TABLE IF NOT EXISTS app_settings (
	id         INTEGER     PRIMARY KEY DEFAULT 1 CHECK (id = 1),
	data       JSONB       NOT NULL DEFAULT '{}',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_settings (
	user_id    TEXT        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	data       JSONB       NOT NULL DEFAULT '{}',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS conversations (
	flow_id    TEXT        NOT NULL,
	scope      TEXT        NOT NULL,
	messages   JSONB       NOT NULL DEFAULT '[]',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (flow_id, scope)
);

CREATE TABLE IF NOT EXISTS organisations (
	id          TEXT        PRIMARY KEY,
	name        TEXT        NOT NULL,
	owner_id    TEXT        NOT NULL,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Must be created after "organisations" (it has a FK to it). Previously this
-- sat before the organisations table and broke a fresh-database migration
-- with "relation organisations does not exist".
CREATE TABLE IF NOT EXISTS org_settings (
	org_id     TEXT        PRIMARY KEY REFERENCES organisations(id) ON DELETE CASCADE,
	data       JSONB       NOT NULL DEFAULT '{}',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS org_members (
	org_id    TEXT        NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
	user_id   TEXT        NOT NULL,
	role      TEXT        NOT NULL,
	joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS org_members_user_id_idx ON org_members (user_id);

CREATE TABLE IF NOT EXISTS org_invites (
	id          TEXT        PRIMARY KEY,
	org_id      TEXT        NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
	email       TEXT        NOT NULL,
	role        TEXT        NOT NULL,
	invited_by  TEXT        NOT NULL,
	token_hash  TEXT        NOT NULL,
	expires_at  TIMESTAMPTZ NOT NULL,
	accepted_at TIMESTAMPTZ,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS org_invites_org_id_idx ON org_invites (org_id);
CREATE INDEX IF NOT EXISTS org_invites_token_hash_idx ON org_invites (token_hash);
-- Only one active (unaccepted) invite per org+email pair.
CREATE UNIQUE INDEX IF NOT EXISTS org_invites_org_email_idx ON org_invites (org_id, email) WHERE accepted_at IS NULL;

-- flow_analysis holds the latest analysis summary per flow, upserted on every
-- analyze run. It backs the welcome dashboard's health/findings cards so they
-- are populated on first load (across sessions and replicas) rather than relying
-- on the per-process in-memory analyzer cache. Owner/org scoping is resolved by
-- JOINing flows at read time, so ownership is never denormalized here.
CREATE TABLE IF NOT EXISTS flow_analysis (
	flow_id      TEXT        PRIMARY KEY REFERENCES flows(id) ON DELETE CASCADE,
	health_score INTEGER     NOT NULL DEFAULT 0,
	errors       INTEGER     NOT NULL DEFAULT 0,
	warnings     INTEGER     NOT NULL DEFAULT 0,
	info         INTEGER     NOT NULL DEFAULT 0,
	by_category  JSONB       NOT NULL DEFAULT '{}',
	analyzed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS flow_collaborators (
	flow_id    TEXT        NOT NULL,
	user_id    TEXT        NOT NULL,
	permission TEXT        NOT NULL DEFAULT 'viewer',
	granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (flow_id, user_id)
);
CREATE INDEX IF NOT EXISTS flow_collaborators_flow_id_idx ON flow_collaborators (flow_id);
-- Composite index for the per-row "is user a collaborator on this flow" EXISTS
-- subquery used in ListFlows when filtering by UserID.
CREATE INDEX IF NOT EXISTS flow_collaborators_flow_user_idx ON flow_collaborators (flow_id, user_id);

CREATE TABLE IF NOT EXISTS refresh_tokens (
	jti        TEXT        PRIMARY KEY,
	user_id    TEXT        NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL,
	revoked    BOOLEAN     NOT NULL DEFAULT FALSE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS refresh_tokens_user_id_idx ON refresh_tokens (user_id);

CREATE TABLE IF NOT EXISTS token_blacklist (
	jti        TEXT        PRIMARY KEY,
	expires_at TIMESTAMPTZ NOT NULL
);

-- identity_links maps external IdP identities (OIDC provider + subject) to
-- local users for SSO login. A user may have links from several providers;
-- one external identity maps to exactly one local user.
CREATE TABLE IF NOT EXISTS identity_links (
	provider   TEXT        NOT NULL,
	subject    TEXT        NOT NULL,
	user_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	email      TEXT        NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (provider, subject)
);
CREATE INDEX IF NOT EXISTS identity_links_user_id_idx ON identity_links (user_id);

-- provider_keys deliberately has NO row-level security. It is an auth-infra
-- table (like refresh_tokens/token_blacklist/api_tokens): every keystore query
-- carries an explicit WHERE user_id = $1, and AES-GCM AAD binds each ciphertext
-- to its (user_id, provider) row so a row-swap can't reuse a stolen key. Adding
-- RLS here would require switching EncryptedKeyStore to BeginRLS and giving the
-- background retention purge a non-request principal.
CREATE TABLE IF NOT EXISTS provider_keys (
	user_id    TEXT        NOT NULL DEFAULT '',
	provider   TEXT        NOT NULL,
	ciphertext TEXT        NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (user_id, provider)
);

CREATE TABLE IF NOT EXISTS usage_metrics (
	id                TEXT        PRIMARY KEY,
	user_id           TEXT        NOT NULL,
	org_id            TEXT        NOT NULL DEFAULT '',
	provider          TEXT        NOT NULL,
	model             TEXT        NOT NULL,
	prompt_tokens     INTEGER     NOT NULL DEFAULT 0,
	completion_tokens INTEGER     NOT NULL DEFAULT 0,
	estimated_cost    FLOAT       NOT NULL DEFAULT 0.0,
	created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS usage_metrics_user_id_idx ON usage_metrics (user_id);
CREATE INDEX IF NOT EXISTS usage_metrics_org_id_idx ON usage_metrics (org_id);
-- The dashboard token-usage chart LEFT JOINs a 14-day generate_series against
-- usage_metrics filtered by (user_id, created_at). The single-column user_id
-- index forces Postgres to scan every row the user has ever logged and filter
-- by date in memory; the composite range-scans just the window.
CREATE INDEX IF NOT EXISTS usage_metrics_user_created_idx ON usage_metrics (user_id, created_at);

CREATE TABLE IF NOT EXISTS knowledge_documents (
	id         TEXT        PRIMARY KEY,
	org_id     TEXT        NOT NULL,
	filename   TEXT        NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS knowledge_chunks (
	id         TEXT        PRIMARY KEY,
	doc_id     TEXT        NOT NULL REFERENCES knowledge_documents(id) ON DELETE CASCADE,
	content    TEXT        NOT NULL,
	embedding  JSONB       NOT NULL -- Using JSONB for portability if pgvector isn't installed
);
CREATE INDEX IF NOT EXISTS knowledge_chunks_doc_id_idx ON knowledge_chunks (doc_id);

-- Migrate a pre-existing provider-only-PK table to the per-user (user_id, provider) PK.
ALTER TABLE provider_keys ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conrelid = 'provider_keys'::regclass AND contype = 'p'
		  AND array_length(conkey, 1) = 1
	) THEN
		ALTER TABLE provider_keys DROP CONSTRAINT provider_keys_pkey;
		ALTER TABLE provider_keys ADD PRIMARY KEY (user_id, provider);
	END IF;
END $$;

-- Add ON DELETE CASCADE from flow_collaborators to flows so deleting a flow
-- does not leave orphaned collaborator rows. Existing deployments created the
-- table without this FK; purge any orphans first (the FK would otherwise fail
-- to validate), then add the constraint only if it isn't already present.
DELETE FROM flow_collaborators c
WHERE NOT EXISTS (SELECT 1 FROM flows f WHERE f.id = c.flow_id);
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conrelid = 'flow_collaborators'::regclass
		  AND contype = 'f'
		  AND conname = 'flow_collaborators_flow_id_fkey'
	) THEN
		ALTER TABLE flow_collaborators
			ADD CONSTRAINT flow_collaborators_flow_id_fkey
			FOREIGN KEY (flow_id) REFERENCES flows(id) ON DELETE CASCADE;
	END IF;
END $$;

-- Purge orphaned rows before adding FKs (existing deployments may have stale data).
DELETE FROM flow_collaborators c WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = c.user_id);

DO $$
BEGIN
	-- flow_collaborators.user_id → users.id  ON DELETE CASCADE
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conrelid = 'flow_collaborators'::regclass AND contype = 'f'
		  AND conname = 'flow_collaborators_user_id_fkey'
	) THEN
		ALTER TABLE flow_collaborators
			ADD CONSTRAINT flow_collaborators_user_id_fkey
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
	END IF;

END $$;

-- NOTE: flows.org_id and flows.owner_id intentionally have NO foreign key.
-- Both use '' as a sentinel ("no org" / "ownerless, read-only"), and '' matches
-- no row in organisations(id)/users(id), so a FK would reject every personal-flow
-- insert; ON DELETE SET NULL also cannot null a NOT NULL column. These
-- relationships are enforced at the service layer. Drop the constraints if an
-- earlier build installed the (broken) versions.
ALTER TABLE flows DROP CONSTRAINT IF EXISTS flows_org_id_fkey;
ALTER TABLE flows DROP CONSTRAINT IF EXISTS flows_owner_id_fkey;

-- NOTE: conversations.flow_id intentionally has NO foreign key to flows. A
-- conversation can exist for a flow that was never persisted to the library
-- (e.g. chatting about an uploaded/unsaved flow), which the storage contract
-- requires both backends to support. A FK (and the matching startup orphan
-- purge) would reject those saves and silently delete legitimate rows. Drop the
-- constraint if an earlier build installed it.
ALTER TABLE conversations DROP CONSTRAINT IF EXISTS conversations_flow_id_fkey;

-- Composite indexes for sorted tenant queries (list flows by owner/org, ordered by recency).
CREATE INDEX IF NOT EXISTS flows_owner_updated_idx ON flows (owner_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS flows_org_updated_idx    ON flows (org_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS knowledge_docs_org_idx   ON knowledge_documents (org_id);

-- Drop redundant indexes (covered by PK or UNIQUE constraint).
DROP INDEX IF EXISTS flow_collaborators_flow_id_idx;
DROP INDEX IF EXISTS flow_collaborators_flow_user_idx;
DROP INDEX IF EXISTS flow_versions_flow_id_idx;

CREATE TABLE IF NOT EXISTS audit_events (
	id            TEXT        PRIMARY KEY,
	user_id       TEXT        NOT NULL DEFAULT '',
	email         TEXT        NOT NULL DEFAULT '',
	action        TEXT        NOT NULL,
	resource_type TEXT        NOT NULL DEFAULT '',
	resource_id   TEXT        NOT NULL DEFAULT '',
	ip            TEXT        NOT NULL DEFAULT '',
	meta          JSONB       NOT NULL DEFAULT '{}',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS audit_events_user_id_idx    ON audit_events (user_id);
CREATE INDEX IF NOT EXISTS audit_events_action_idx     ON audit_events (action);
CREATE INDEX IF NOT EXISTS audit_events_created_at_idx ON audit_events (created_at DESC);

CREATE TABLE IF NOT EXISTS flow_versions (
	id          TEXT        PRIMARY KEY,
	flow_id     TEXT        NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
	version     INTEGER     NOT NULL,
	comment     TEXT        NOT NULL DEFAULT '',
	content     JSONB       NOT NULL DEFAULT '{}',
	metadata    JSONB       NOT NULL DEFAULT '{}',
	created_by  TEXT        NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE (flow_id, version)
);
CREATE INDEX IF NOT EXISTS flow_versions_flow_id_idx ON flow_versions (flow_id);

-- ──────────────────────────────────────────────────────────────────────────────
-- Row-Level Security (defense-in-depth)
--
-- RLS policies supplement the Go-layer authz checks. If a code path
-- accidentally omits a WHERE clause, RLS prevents cross-tenant data leaks at
-- the database level. The policies read the session variable set by the RLS
-- middleware (app.current_user_id). When the variable is empty (migrations,
-- local mode, superuser), all rows are visible so existing operations are not
-- disrupted.
-- ──────────────────────────────────────────────────────────────────────────────

-- app_current_user_id() returns the per-request user id the Go RLS middleware
-- installs via set_config('app.current_user_id', ...). It is NULL/'' when unset.
CREATE OR REPLACE FUNCTION app_current_user_id() RETURNS text
    LANGUAGE sql STABLE AS $fn$ SELECT current_setting('app.current_user_id', true) $fn$;

-- app_rls_active() is true only when a non-empty user context is set; every
-- policy short-circuits to "allow" when it is false (migrations, local mode,
-- background jobs, superuser). This MUST be a coalesce()-based check rather than
-- an inline current_setting(...) = '' comparison: an unset GUC returns NULL, and
-- NULL = '' is NULL, which RLS treats as deny — so the naive form would hide
-- every row on any pooled query that runs without a user context.
CREATE OR REPLACE FUNCTION app_rls_active() RETURNS boolean
    LANGUAGE sql STABLE AS $fn$ SELECT coalesce(current_setting('app.current_user_id', true), '') <> '' $fn$;

-- Enable AND force RLS on every tenant-scoped table. FORCE is required because
-- the application connects as the role that owns these tables, and a table owner
-- bypasses non-forced RLS entirely.
ALTER TABLE flows               ENABLE ROW LEVEL SECURITY;
ALTER TABLE flows               FORCE  ROW LEVEL SECURITY;
ALTER TABLE conversations       ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversations       FORCE  ROW LEVEL SECURITY;
ALTER TABLE flow_versions       ENABLE ROW LEVEL SECURITY;
ALTER TABLE flow_versions       FORCE  ROW LEVEL SECURITY;
ALTER TABLE knowledge_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_documents FORCE  ROW LEVEL SECURITY;
ALTER TABLE knowledge_chunks    ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_chunks    FORCE  ROW LEVEL SECURITY;

-- flow_collaborators is a GRANT table (like org_members): every other table's
-- policy reads it to test "is the caller a collaborator?". Giving it its own RLS
-- policy makes flows ⇄ flow_collaborators mutually recursive, which Postgres
-- rejects at query time ("infinite recursion detected in policy"). It also would
-- be self-defeating — a policy hiding the very rows the membership check needs.
-- So it is deliberately left WITHOUT RLS; access is gated at the service layer.
ALTER TABLE flow_collaborators  DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_flow_collaborators_visible ON flow_collaborators;

-- Policies are dropped and recreated on every migration so that body changes
-- always take effect. (CREATE POLICY has no OR REPLACE form, and an earlier
-- IF NOT EXISTS guard would pin stale bodies on already-migrated deployments.)

-- ── flows: owner, collaborator, or org member ──
DROP POLICY IF EXISTS rls_flows_visible ON flows;
CREATE POLICY rls_flows_visible ON flows FOR ALL USING (
    NOT app_rls_active()
    OR owner_id = app_current_user_id()
    OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = flows.id AND fc.user_id = app_current_user_id())
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = flows.org_id AND om.user_id = app_current_user_id())
) WITH CHECK (
    NOT app_rls_active()
    OR owner_id = app_current_user_id()
    OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = flows.id AND fc.user_id = app_current_user_id())
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = flows.org_id AND om.user_id = app_current_user_id())
);

-- ── conversations: inherit the parent flow's visibility ──
DROP POLICY IF EXISTS rls_conversations_visible ON conversations;
CREATE POLICY rls_conversations_visible ON conversations FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = conversations.flow_id
          AND ( f.owner_id = app_current_user_id()
             OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
             OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()) )
    )
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = conversations.flow_id
          AND ( f.owner_id = app_current_user_id()
             OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
             OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()) )
    )
);

-- ── flow_versions: inherit the parent flow's visibility ──
DROP POLICY IF EXISTS rls_flow_versions_visible ON flow_versions;
CREATE POLICY rls_flow_versions_visible ON flow_versions FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = flow_versions.flow_id
          AND ( f.owner_id = app_current_user_id()
             OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
             OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()) )
    )
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = flow_versions.flow_id
          AND ( f.owner_id = app_current_user_id()
             OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
             OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()) )
    )
);

-- ── knowledge_documents: org-scoped ──
DROP POLICY IF EXISTS rls_knowledge_docs_visible ON knowledge_documents;
CREATE POLICY rls_knowledge_docs_visible ON knowledge_documents FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = knowledge_documents.org_id AND om.user_id = app_current_user_id())
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = knowledge_documents.org_id AND om.user_id = app_current_user_id())
);

-- ── knowledge_chunks: inherit the parent document's org ──
DROP POLICY IF EXISTS rls_knowledge_chunks_visible ON knowledge_chunks;
CREATE POLICY rls_knowledge_chunks_visible ON knowledge_chunks FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM knowledge_documents kd
        WHERE kd.id = knowledge_chunks.doc_id
          AND EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = kd.org_id AND om.user_id = app_current_user_id())
    )
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM knowledge_documents kd
        WHERE kd.id = knowledge_chunks.doc_id
          AND EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = kd.org_id AND om.user_id = app_current_user_id())
    )
);

-- ── flow_analysis: add per-rule finding distribution ──
ALTER TABLE flow_analysis ADD COLUMN IF NOT EXISTS by_rule JSONB NOT NULL DEFAULT '{}';

-- ── flow_analysis_history: append-only time series for trend charts ──
CREATE TABLE IF NOT EXISTS flow_analysis_history (
    id           TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    flow_id      TEXT        NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    health_score INTEGER     NOT NULL DEFAULT 0,
    errors       INTEGER     NOT NULL DEFAULT 0,
    warnings     INTEGER     NOT NULL DEFAULT 0,
    info         INTEGER     NOT NULL DEFAULT 0,
    by_rule      JSONB       NOT NULL DEFAULT '{}',
    analyzed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS flow_analysis_history_flow_idx ON flow_analysis_history (flow_id, analyzed_at DESC);
CREATE INDEX IF NOT EXISTS flow_analysis_history_when_idx ON flow_analysis_history (analyzed_at DESC);

-- RLS: a user can see history rows for flows they own or collaborate on.
ALTER TABLE flow_analysis_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE flow_analysis_history FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_flow_analysis_history_visible ON flow_analysis_history;
CREATE POLICY rls_flow_analysis_history_visible ON flow_analysis_history FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = flow_analysis_history.flow_id
          AND (f.owner_id = app_current_user_id()
               OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
               OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
    )
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = flow_analysis_history.flow_id
          AND (f.owner_id = app_current_user_id()
               OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
               OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
    )
);

-- ── api_tokens: machine credentials (PATs). No RLS — like refresh_tokens, this
-- is auth infrastructure looked up by hash during the request BEFORE any user
-- context exists; access is scoped by user_id in the query and by the secret hash. ──
CREATE TABLE IF NOT EXISTS api_tokens (
    id         TEXT        PRIMARY KEY,
    user_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT        NOT NULL DEFAULT '',
    token_hash TEXT        UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS api_tokens_user_idx ON api_tokens (user_id);

-- ── user_tokens: one-shot credentials for password reset & email verification.
-- No RLS — like api_tokens, this is auth infrastructure looked up by hash before
-- a user context exists; the secret hash is the access control. ──
CREATE TABLE IF NOT EXISTS user_tokens (
    token_hash TEXT        PRIMARY KEY,
    purpose    TEXT        NOT NULL,
    user_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS user_tokens_user_idx ON user_tokens (user_id);

-- ── finding_status: persistent, team-shared triage state, one row per finding ──
CREATE TABLE IF NOT EXISTS finding_status (
    flow_id       TEXT        NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    finding_key   TEXT        NOT NULL,
    rule_id       TEXT        NOT NULL DEFAULT '',
    status        TEXT        NOT NULL DEFAULT 'open',
    justification TEXT        NOT NULL DEFAULT '',
    assignee_id   TEXT        NOT NULL DEFAULT '',
    updated_by    TEXT        NOT NULL DEFAULT '',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (flow_id, finding_key)
);

-- ── flow_baselines: accepted set of finding keys per flow (one row per flow) ──
CREATE TABLE IF NOT EXISTS flow_baselines (
    flow_id    TEXT        PRIMARY KEY REFERENCES flows(id) ON DELETE CASCADE,
    keys       JSONB       NOT NULL DEFAULT '[]',
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- RLS: triage and baseline rows follow their flow's visibility (owner,
-- collaborator, or org member) — identical to flow_analysis_history above.
ALTER TABLE finding_status ENABLE ROW LEVEL SECURITY;
ALTER TABLE finding_status FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_finding_status_visible ON finding_status;
CREATE POLICY rls_finding_status_visible ON finding_status FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = finding_status.flow_id
          AND (f.owner_id = app_current_user_id()
               OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
               OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
    )
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = finding_status.flow_id
          AND (f.owner_id = app_current_user_id()
               OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
               OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
    )
);

ALTER TABLE flow_baselines ENABLE ROW LEVEL SECURITY;
ALTER TABLE flow_baselines FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_flow_baselines_visible ON flow_baselines;
CREATE POLICY rls_flow_baselines_visible ON flow_baselines FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = flow_baselines.flow_id
          AND (f.owner_id = app_current_user_id()
               OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
               OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
    )
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = flow_baselines.flow_id
          AND (f.owner_id = app_current_user_id()
                OR EXISTS (SELECT 1 from flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
                OR EXISTS (SELECT 1 from org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
     )
);
`

const policiesSQL = `
CREATE TABLE IF NOT EXISTS policies (
    id            TEXT        NOT NULL,
    org_id        TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    description   TEXT        DEFAULT '',
    rules         JSONB       NOT NULL DEFAULT '[]',
    gate_severity TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, org_id)
);

CREATE INDEX IF NOT EXISTS policies_org_id_idx ON policies (org_id);

ALTER TABLE policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE policies FORCE ROW LEVEL SECURITY;

CREATE POLICY rls_policies_visible ON policies FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = policies.org_id AND om.user_id = app_current_user_id())
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = policies.org_id AND om.user_id = app_current_user_id())
);
`

// governanceAlertsSQL (v12) adds the in-app governance alerts inbox. The
// continuous-governance scanner writes a row whenever it detects baseline drift
// or a health regression on a flow; the notifications bell reads them. Alerts
// are team-shared and inherit the parent flow's visibility (same RLS shape as
// finding_status), so a user sees alerts only for flows they own / collaborate
// on / share an org with. read_at/dismissed_at are team-global (ack semantics:
// one team member acknowledging clears the badge for the whole team). An index on
// created_at DESC backs the newest-first list query.
const governanceAlertsSQL = `
CREATE TABLE IF NOT EXISTS gov_alerts (
    id            TEXT        PRIMARY KEY,
    flow_id       TEXT        NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    org_id        TEXT        NOT NULL DEFAULT '',
    type          TEXT        NOT NULL,
    title         TEXT        NOT NULL DEFAULT '',
    message       TEXT        NOT NULL DEFAULT '',
    severity      TEXT        NOT NULL DEFAULT 'warning',
    new_errors    INT         NOT NULL DEFAULT 0,
    new_warnings  INT         NOT NULL DEFAULT 0,
    health_score  INT         NOT NULL DEFAULT 0,
    prev_health   INT         NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    read_at       TIMESTAMPTZ,
    dismissed_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS gov_alerts_created_idx ON gov_alerts (created_at DESC);

ALTER TABLE gov_alerts ENABLE ROW LEVEL SECURITY;
ALTER TABLE gov_alerts FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS rls_gov_alerts_visible ON gov_alerts;
CREATE POLICY rls_gov_alerts_visible ON gov_alerts FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = gov_alerts.flow_id
          AND (f.owner_id = app_current_user_id()
               OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
               OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
    )
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (
        SELECT 1 FROM flows f
        WHERE f.id = gov_alerts.flow_id
          AND (f.owner_id = app_current_user_id()
               OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
               OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
    )
);
`

// governanceAlertsDownSQL reverses v12. Data-lossy: the alerts inbox is
// destroyed (the scanner re-populates it on the next sweep after re-upgrade).
const governanceAlertsDownSQL = `
DROP TABLE IF EXISTS gov_alerts;
`

// flowsNameTrgmSQL (v13) adds a trigram GIN index backing the library's
// name-substring search (`name ILIKE '%q%'`). A leading-wildcard ILIKE cannot
// use a btree index, so the query filtered every in-scope row's name after the
// owner/org index scan — fine for small personal libraries, a full scan of
// every org flow's name for large orgs. pg_trgm's GIN index makes the
// substring match index-backed. CREATE EXTENSION is idempotent (IF NOT
// EXISTS); on managed Postgres (Azure Database) pg_trgm is preinstalled and
// creatable by the table owner.
const flowsNameTrgmSQL = `
CREATE EXTENSION IF NOT EXISTS pg_trgm;
DROP INDEX IF EXISTS flows_name_trgm_idx;
CREATE INDEX flows_name_trgm_idx ON flows USING gin (name gin_trgm_ops);
`

// flowsNameTrgmDownSQL reverses v13. The extension itself is left installed
// (other objects may depend on it; dropping an extension other users rely on
// from a down-migration would be surprising).
const flowsNameTrgmDownSQL = `
DROP INDEX IF EXISTS flows_name_trgm_idx;
`

// apiTokenScopesSQL (R2-1): PAT capability restriction. scopes is a
// comma-joined TEXT column (the set is ≤4 short, comma-free names — an array
// type would drag a pq dependency for no benefit). ” = unscoped = full
// access (every pre-existing token), so the default keeps behavior identical
// until a scoped token is minted.
//
//nolint:gosec // G101 false positive: schema DDL, not a credential
const apiTokenScopesSQL = `
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS scopes TEXT NOT NULL DEFAULT '';
`

//nolint:gosec // G101 false positive: schema DDL, not a credential
const apiTokenScopesDownSQL = `
ALTER TABLE api_tokens DROP COLUMN IF EXISTS scopes;
`

// flowTagsSQL (R2-4): organizational tagging for the flow library — business
// unit, criticality, environment. CSV TEXT column: tags are a small set of
// normalized (comma-free, ≤32 char) names; filtering uses delimiter-anchored
// LIKE so 'prod' never matches 'production'.
const flowTagsSQL = `
ALTER TABLE flows ADD COLUMN IF NOT EXISTS tags TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS flows_tags_idx ON flows (tags);
`

const flowTagsDownSQL = `
DROP INDEX IF EXISTS flows_tags_idx;
ALTER TABLE flows DROP COLUMN IF EXISTS tags;
`

// govAlertTargetsSQL (R2-5): targeted alerts. target_user_id ” (default) =
// team-wide (every existing alert); a user ID = visible ONLY to that user —
// the delivery target for assignment/comment notifications, which are
// personal, not governance-wide.
const govAlertTargetsSQL = `
ALTER TABLE gov_alerts ADD COLUMN IF NOT EXISTS target_user_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS gov_alerts_target_user_idx ON gov_alerts (target_user_id) WHERE target_user_id <> '';
`

const govAlertTargetsDownSQL = `
DROP INDEX IF EXISTS gov_alerts_target_user_idx;
ALTER TABLE gov_alerts DROP COLUMN IF EXISTS target_user_id;
`

// orgChannelsSQL (R2-3): per-org notification channels. Governance events for
// an org's flows fan out to the org's own channels IN ADDITION to the
// deployment-global ones — routing by ownership instead of one global blast.
// RLS mirrors the knowledge tables (org-member read; writes also via member
// policy — the HTTP layer additionally requires admin).
const orgChannelsSQL = `
CREATE TABLE IF NOT EXISTS org_channels (
    id         TEXT        PRIMARY KEY,
    org_id     TEXT        NOT NULL,
    name       TEXT        NOT NULL DEFAULT '',
    kind       TEXT        NOT NULL,
    url        TEXT        NOT NULL,
    secret     TEXT        NOT NULL DEFAULT '',
    enabled    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS org_channels_org_idx ON org_channels (org_id);

ALTER TABLE org_channels ENABLE ROWS LEVEL SECURITY;

DROP POLICY IF EXISTS rls_org_channels_visible ON org_channels;
CREATE POLICY rls_org_channels_visible ON org_channels FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_channels.org_id AND om.user_id = app_current_user_id())
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_channels.org_id AND om.user_id = app_current_user_id())
);
`

const orgChannelsDownSQL = `
DROP TABLE IF EXISTS org_channels;
`

// flowsContentTrgmSQL (R3-5a) backs library content-search pushdown: an
// ILIKE over content::text finds flows whose BLOCK NAMES/properties mention
// the needle WITHOUT loading + parsing every flow in-process (the previous
// SearchLibrary capped at 50 flows — on a 500-flow org, 450 flows were
// silently invisible to search). pg_trgm (required since v13) makes the scan
// index-backed. NOTE: deployments with Azure Blob content offloading store a
// "{}" placeholder in this column — the storage method detects that mode and
// reports unsupported, so the service falls back to the legacy scan there.
const flowsContentTrgmSQL = `
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX IF NOT EXISTS flows_content_trgm_idx ON flows USING gin ((content::text) gin_trgm_ops);
`

const flowsContentTrgmDownSQL = `
DROP INDEX IF EXISTS flows_content_trgm_idx;
`
