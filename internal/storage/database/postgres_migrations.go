package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// migrationChecksum returns the hex SHA-256 of a migration's SQL, recorded at apply time and re-verified on boot.
func migrationChecksum(sqlText string) string {
	sum := sha256.Sum256([]byte(sqlText))
	return hex.EncodeToString(sum[:])
}

// ---- Schema migration ----

// migrationLockKey is the pg_advisory_lock key serializing schema migrations; any fixed app-unique value works.
const migrationLockKey int64 = 0x70616461_6E616C7A // "padanalz" in hex bytes

// migration is a single forward-only schema step, applied at most once under the advisory lock.
// No automatic rollback: DDL is kept forward-compatible by discipline (IF NOT EXISTS, additive columns).
type migration struct {
	version int
	name    string
	sql     string
	// downSQL is the optional reverse of sql; empty means intentionally irreversible
	// (MigrateDown refuses to cross it). Never checksummed, never run on boot.
	downSQL string
	// compat is recorded with the step; the zero value counts as breaking (see migrationCompat).
	compat migrationCompat
}

// migrationCompat says whether the release before a step can still run against a schema that includes it
// (migrations/README.md). It is recorded in schema_migrations so an OLDER binary can read it.
type migrationCompat string

const (
	// compatBreaking: older binaries must refuse the schema. The frozen Go steps (v1–v31) leave compat empty and
	// count as breaking; no binary that checks compat predates them, so their value is never consulted.
	compatBreaking migrationCompat = "breaking"
	// compatExpand: additive only, so rolling the image back needs no schema rollback.
	compatExpand migrationCompat = "expand"
)

// recordedCompat is the value stored in schema_migrations.compat.
func (m migration) recordedCompat() migrationCompat {
	if m.compat == compatExpand {
		return compatExpand
	}
	return compatBreaking
}

// migrations is the ordered, append-only set of schema versions: the frozen Go literals, then the embedded .sql
// steps from v32 on (migrations/README.md).
var migrations = append(goMigrations[:len(goMigrations):len(goMigrations)],
	mustLoadSQLMigrations(goMigrations[len(goMigrations)-1].version)...)

// goMigrations are v1–v31 (v1 is the baseline). Frozen: new steps are .sql files in migrations/.
var goMigrations = []migration{
	{version: 1, name: "baseline", sql: schemaBaseline, downSQL: ""},
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
	{version: 19, name: "rls_force_all_tables", sql: rlsForceSQL, downSQL: rlsForceDownSQL},
	{version: 20, name: "pgvector_knowledge_repair", sql: pgvectorRepairSQL, downSQL: pgvectorRepairDownSQL},
	{version: 21, name: "org_custom_rules", sql: orgCustomRulesSQL, downSQL: orgCustomRulesDownSQL},
	{version: 22, name: "usage_served_model", sql: usageServedModelSQL, downSQL: usageServedModelDownSQL},
	{version: 23, name: "org_provider_keys", sql: orgProviderKeysSQL, downSQL: orgProviderKeysDownSQL},
	{version: 24, name: "org_provider_keys_secret_set_by", sql: orgProviderKeysSecretSetBySQL, downSQL: orgProviderKeysSecretSetByDownSQL},
	{version: 25, name: "azure_registry", sql: azureRegistrySQL, downSQL: azureRegistryDownSQL},
	{version: 26, name: "perf_indexes_v26", sql: perfIndexesV26SQL, downSQL: perfIndexesV26DownSQL},
	{version: 27, name: "perf_round2_v27", sql: perfRound2V27SQL, downSQL: perfRound2V27DownSQL},
	{version: 28, name: "perf_round3_v28", sql: perfRound3V28SQL, downSQL: perfRound3V28DownSQL},
	{version: 29, name: "perf_round4_v29", sql: perfRound4V29SQL, downSQL: perfRound4V29DownSQL},
	{version: 30, name: "perf_indexes_v30", sql: perfIndexesV30SQL, downSQL: perfIndexesV30DownSQL},
	{version: 31, name: "users_created_id_idx", sql: usersCreatedIdIdxV31SQL, downSQL: usersCreatedIdIdxV31DownSQL},
}

// perfIndexesV30SQL (v30) adds the `id` tiebreaker to the gov_alerts and block-count indexes so their ORDER BYs are fully index-ordered.
const perfIndexesV30SQL = `
DROP INDEX IF EXISTS flows_blockcount_updated_idx;
CREATE INDEX IF NOT EXISTS flows_blockcount_updated_idx
    ON flows ((COALESCE((metadata->>'BlockCount')::int, 0)) DESC, updated_at DESC, id);

DROP INDEX IF EXISTS gov_alerts_created_idx;
CREATE INDEX IF NOT EXISTS gov_alerts_created_id_idx ON gov_alerts (created_at DESC, id);
`

// perfIndexesV30DownSQL reverses v30: restores both pre-v30 definitions. Idempotent.
const perfIndexesV30DownSQL = `
DROP INDEX IF EXISTS gov_alerts_created_id_idx;
CREATE INDEX IF NOT EXISTS gov_alerts_created_idx ON gov_alerts (created_at DESC);

DROP INDEX IF EXISTS flows_blockcount_updated_idx;
CREATE INDEX IF NOT EXISTS flows_blockcount_updated_idx
    ON flows ((COALESCE((metadata->>'BlockCount')::int, 0)) DESC, updated_at DESC);
`

// usersCreatedIdIdxV31SQL (v31) backs the admin user listing's ORDER BY (created_at DESC, id ASC) — previously a full scan + sort.
const usersCreatedIdIdxV31SQL = `
CREATE INDEX IF NOT EXISTS users_created_id_idx ON users (created_at DESC, id);
`

// usersCreatedIdIdxV31DownSQL reverses v31. Idempotent.
const usersCreatedIdIdxV31DownSQL = `
DROP INDEX IF EXISTS users_created_id_idx;
`

// flowBlockCountIndexSQL adds an expression index matching FlowSortBlocksDesc ORDER BY (was a full scan + sort; safe cast — BlockCount is a JSON number or absent).
const flowBlockCountIndexSQL = `
CREATE INDEX IF NOT EXISTS flows_blockcount_updated_idx
    ON flows ((COALESCE((metadata->>'BlockCount')::int, 0)) DESC, updated_at DESC);
`

// flowVersionsBlobKeySQL stores the content blob's key per version row so SaveFlowVersion can upload BEFORE the flow's FOR UPDATE lock; legacy rows keep "".
const flowVersionsBlobKeySQL = `
ALTER TABLE flow_versions ADD COLUMN IF NOT EXISTS blob_key TEXT NOT NULL DEFAULT '';
`

// flowVersionsBlobKeyDownSQL reverses v10. Data-lossy (stored blob key discarded; reads fall back to the derived key).
const flowVersionsBlobKeyDownSQL = `
ALTER TABLE flow_versions DROP COLUMN IF EXISTS blob_key;
`

// flowBlockCountIndexDownSQL reverses v9 (drop the expression index).
const flowBlockCountIndexDownSQL = `
DROP INDEX IF EXISTS flows_blockcount_updated_idx;
`

// policiesDownSQL reverses v8 (drops table, index + RLS policies). Data-lossy: policies destroyed.
const policiesDownSQL = `
DROP TABLE IF EXISTS policies;
`

// usageMetricsOrgIndexDownSQL reverses v7: drop the composite, restore the single-column org_id index.
const usageMetricsOrgIndexDownSQL = `
DROP INDEX IF EXISTS usage_metrics_org_created_idx;
CREATE INDEX IF NOT EXISTS usage_metrics_org_id_idx ON usage_metrics (org_id);
`

// perfIndexesDownSQL reverses v6: drop the three composites, restore the six redundant single-column indexes.
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

// findingStatusCreatedAtDownSQL reverses v5. Data-lossy: the MTTR created_at column (and data) is dropped.
const findingStatusCreatedAtDownSQL = `
ALTER TABLE finding_status DROP COLUMN IF EXISTS created_at;
`

// flowAnalysisV2DownSQL reverses v4: drop the six dashboard-rollup columns. Data-lossy.
const flowAnalysisV2DownSQL = `
ALTER TABLE flow_analysis DROP COLUMN IF EXISTS by_confidence;
ALTER TABLE flow_analysis DROP COLUMN IF EXISTS auto_fixable_count;
ALTER TABLE flow_analysis DROP COLUMN IF EXISTS total_findings;
ALTER TABLE flow_analysis_history DROP COLUMN IF EXISTS by_confidence;
ALTER TABLE flow_analysis_history DROP COLUMN IF EXISTS auto_fixable_count;
ALTER TABLE flow_analysis_history DROP COLUMN IF EXISTS total_findings;
`

// refreshSessionDdlDownSQL reverses v3: drop the device-info columns. Data-lossy (UA/IP labels destroyed).
const refreshSessionDdlDownSQL = `
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS user_agent;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS ip;
`

// commentsAndSharesDownSQL reverses v2: drop share_tokens and finding_comments. Data-lossy.
const commentsAndSharesDownSQL = `
DROP TABLE IF EXISTS share_tokens;
DROP TABLE IF EXISTS finding_comments;
`

// pgvectorKnowledgeSQL enables server-side similarity search. The column is intentionally DIMENSIONLESS:
// the app enforces the configured dimension at insert (wrong-dimension chunks stay NULL — the "re-index
// after changing the embedding provider" contract). No-ops without the extension (Go-side cosine ranker).
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

// pgvectorKnowledgeDownSQL reverses v11: drop embedding_vec (HNSW goes with it) but NOT the vector EXTENSION.
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

// findingStatusCreatedAtSQL adds created_at to finding_status for MTTR / stale counts; existing rows backfill to NOW() and are excluded via `updated_at >= created_at`.
const findingStatusCreatedAtSQL = `
ALTER TABLE finding_status ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
`

// perfIndexesSQL adds composite indexes eliminating full-table scans on hot paths and drops redundant single-column indexes covered by a composite/UNIQUE leftmost prefix.
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

// usageMetricsOrgIndexSQL adds a composite (org_id, created_at) index for the AI budget-check hot path; the single-column org_id index is dropped (leftmost prefix).
const usageMetricsOrgIndexSQL = `
CREATE INDEX IF NOT EXISTS usage_metrics_org_created_idx ON usage_metrics (org_id, created_at);
DROP INDEX IF EXISTS usage_metrics_org_id_idx;
`

// flowAnalysisV2SQL adds dashboard-rollup columns to flow_analysis (+ history mirror) for the org-wide confidence/fix-availability KPIs; additive.
const flowAnalysisV2SQL = `
ALTER TABLE flow_analysis ADD COLUMN IF NOT EXISTS by_confidence JSONB NOT NULL DEFAULT '{}';
ALTER TABLE flow_analysis ADD COLUMN IF NOT EXISTS auto_fixable_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE flow_analysis ADD COLUMN IF NOT EXISTS total_findings INTEGER NOT NULL DEFAULT 0;

ALTER TABLE flow_analysis_history ADD COLUMN IF NOT EXISTS by_confidence JSONB NOT NULL DEFAULT '{}';
ALTER TABLE flow_analysis_history ADD COLUMN IF NOT EXISTS auto_fixable_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE flow_analysis_history ADD COLUMN IF NOT EXISTS total_findings INTEGER NOT NULL DEFAULT 0;
`

// refreshSessionDdlSQL records the User-Agent and client IP a refresh token was issued to (active-sessions UI).
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

// migrate applies every pending migration under a session-level advisory lock (dedicated connection)
// so concurrent replicas serialize. Each step + its version record run in one transaction: a failure rolls both back.
func (b *PostgresStorageBackend) migrate(ctx context.Context) error {
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Close()

	restore, err := liftSessionTimeouts(ctx, conn)
	if err != nil {
		return err
	}
	defer restore()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer releaseMigrationLock(conn)

	if err := ensureMigrationsTable(ctx, conn); err != nil {
		return fmt.Errorf("ensure schema_migrations table: %w", err)
	}

	current, err := currentVersion(ctx, conn)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	// A schema newer than this binary is accepted only when every newer step was recorded as expand-only
	// (the previous release keeps working on it, so an image rollback needs no schema rollback). Anything else,
	// including steps applied before compat was recorded, is refused: the old binary would run queries against
	// a shape it has never seen. Roll back first (`bakicli migrate down`) or deploy newer.
	latest := migrations[len(migrations)-1].version
	if current > latest {
		newer, err := appliedStepsAfter(ctx, conn, latest)
		if err != nil {
			return fmt.Errorf("read schema steps newer than this binary: %w", err)
		}
		if breaking := breakingSteps(newer); len(breaking) > 0 {
			return fmt.Errorf("schema is at v%d, newer than the v%d this binary knows, and %s cannot run under an older binary: refusing to run against an unknown schema (roll the schema back to v%d with `bakicli migrate down %d`, or deploy a binary that knows v%d)",
				current, latest, strings.Join(breaking, ", "), latest, latest, current)
		}
		slog.Warn("schema is ahead of this binary by expand-only steps; running against it", "schema_version", current, "binary_version", latest)
	}

	// Integrity gate: fail boot when an applied migration's SQL no longer matches its checksum.
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

	if applied == 0 {
		slog.Info("schema up to date", "version", current)
	} else {
		slog.Info("schema migrations complete", "version", current)
	}
	return nil
}

// liftSessionTimeouts clears the pool's statement/lock timeouts on the migration's dedicated session
// and returns a restorer: a long CREATE INDEX can outrun the statement budget, and a replica waiting on
// the advisory lock must not be killed by lock_timeout.
func liftSessionTimeouts(ctx context.Context, conn *sql.Conn) (restore func(), err error) {
	if _, err := conn.ExecContext(ctx, "SET statement_timeout = 0"); err != nil {
		return nil, fmt.Errorf("lift statement_timeout for migration: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET lock_timeout = 0"); err != nil {
		return nil, fmt.Errorf("lift lock_timeout for migration: %w", err)
	}
	return func() {
		// Fresh context: the caller's may already be cancelled, and a pooled conn with the
		// timeouts still lifted is the bug.
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(rctx, "RESET statement_timeout"); err != nil {
			slog.Warn("migration: RESET statement_timeout failed; closing connection", "err", err)
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			return
		}
		if _, err := conn.ExecContext(rctx, "RESET lock_timeout"); err != nil {
			slog.Warn("migration: RESET lock_timeout failed; closing connection", "err", err)
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}, nil
}

// releaseMigrationLock releases the session-level lock on a fresh context before conn returns to the
// pool (a pooled conn keeps its session, so a held lock would block every other replica); on failure the conn is discarded — ending the session frees the lock.
func releaseMigrationLock(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
		slog.Warn("migration: advisory unlock failed; closing connection", "err", err)
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// ensureMigrationsTable creates the version-tracking table; deployments predating versioning get it on first boot with baseline recorded as v1.
func ensureMigrationsTable(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER    PRIMARY KEY,
		name       TEXT       NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return err
	}
	// checksum records the step SQL's SHA-256, added additively; pre-checksum rows default to ''
	// and are backfilled by verifyChecksums rather than treated as drift.
	if _, err := conn.ExecContext(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// compat records whether an older binary may run on the schema (see migrationCompat); rows written before
	// the column existed default to the safe answer.
	_, err := conn.ExecContext(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS compat TEXT NOT NULL DEFAULT 'breaking'`)
	return err
}

// appliedStep is one schema_migrations row as an older binary sees it.
type appliedStep struct {
	version int
	name    string
	compat  migrationCompat
}

// appliedStepsAfter returns the recorded steps above version, in order.
func appliedStepsAfter(ctx context.Context, conn *sql.Conn, version int) ([]appliedStep, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version, name, compat FROM schema_migrations WHERE version > $1 ORDER BY version`, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []appliedStep
	for rows.Next() {
		var s appliedStep
		if err := rows.Scan(&s.version, &s.name, &s.compat); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// breakingSteps names the steps an older binary cannot run under.
func breakingSteps(steps []appliedStep) []string {
	var out []string
	for _, s := range steps {
		if s.compat != compatExpand {
			out = append(out, fmt.Sprintf("v%d %q", s.version, s.name))
		}
	}
	return out
}

// verifyChecksums compares each applied migration's recorded checksum against the current SQL; empty (pre-checksum) values are adopted, a mismatch is fatal drift.
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

// loadAppliedChecksums returns version → recorded checksum for every schema_migrations row.
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

// A step waits at most migrationLockTimeout for each table lock, then rolls back and retries. With no limit, a
// step queued behind a long transaction of the still-serving revision, and every query on that table queued
// behind the step. The session's advisory-lock wait stays unbounded (lock_timeout is only SET LOCAL here).
var (
	migrationLockTimeout  = 5 * time.Second
	migrationLockAttempts = 6
	migrationLockBackoff  = 2 * time.Second
)

// pgLockNotAvailable is SQLSTATE lock_not_available, raised when lock_timeout expires.
const pgLockNotAvailable = "55P03"

// applyMigration applies one step, retrying (with a growing pause) while a table lock is not available.
func applyMigration(ctx context.Context, conn *sql.Conn, m migration) error {
	for attempt := 1; ; attempt++ {
		err := applyMigrationOnce(ctx, conn, m)
		if err == nil || !isPgErrCode(err, pgLockNotAvailable) || attempt >= migrationLockAttempts {
			return err
		}
		wait := migrationLockBackoff * time.Duration(attempt)
		slog.Warn("schema migration could not get a table lock in time; retrying",
			"version", m.version, "name", m.name, "attempt", attempt, "retry_in", wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// applyMigrationOnce runs one step's SQL and records its version in a single transaction, so a half-applied step can never be marked complete.
func applyMigrationOnce(ctx context.Context, conn *sql.Conn, m migration) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", migrationLockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set lock_timeout: %w", err)
	}
	if _, err = tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("exec sql: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, compat) VALUES ($1, $2, $3, $4)`,
		m.version, m.name, migrationChecksum(m.sql), string(m.recordedCompat())); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	return tx.Commit()
}

// applyMigrationDown runs one step's downSQL and removes its version record in one transaction; the downSQL is not checksummed.
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
	// Reversible is false when downSQL is empty (intentionally irreversible).
	Reversible bool
}

// MigrationSteps exposes the ordered step list + reversibility for bakicli's `migrate down` planning.
func MigrationSteps() []MigrationStep {
	out := make([]MigrationStep, 0, len(migrations))
	for _, m := range migrations {
		out = append(out, MigrationStep{Version: m.version, Name: m.name, Reversible: m.downSQL != ""})
	}
	return out
}

// ErrMigrationNotReversible is returned by MigrateDown when the rollback path crosses a step with no downSQL (e.g. the baseline); restore from backup instead.
var ErrMigrationNotReversible = fmt.Errorf("migration not reversible: a step in the rollback path has no down-migration (baseline or unsafe); restore from backup instead")

// MigrateDown rolls the schema back to targetVersion via each step's downSQL in strict descending
// order, each in its own transaction under the advisory lock. Operator-invoked (bakicli), never on boot.
func (b *PostgresStorageBackend) MigrateDown(ctx context.Context, targetVersion int) ([]int, error) {
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Close()

	restore, err := liftSessionTimeouts(ctx, conn)
	if err != nil {
		return nil, err
	}
	defer restore()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return nil, fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer releaseMigrationLock(conn)

	current, err := currentVersion(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	if targetVersion >= current {
		return nil, nil // nothing to do (already at or below target)
	}

	// Pre-check the whole rollback path before any destructive work, so a "not reversible"
	// failure leaves no half-rolled-back schema.
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

// CurrentSchemaVersion reports the highest applied migration version, or 0 with no backend/table.
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

// governanceAlertsSQL (v12) adds the in-app governance alerts inbox: team-shared, inherit flow visibility (same RLS shape as finding_status); read/dismiss stamps are team-global.
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

// governanceAlertsDownSQL reverses v12. Data-lossy: the inbox is destroyed (the scanner repopulates it after re-upgrade).
const governanceAlertsDownSQL = `
DROP TABLE IF EXISTS gov_alerts;
`

// flowsNameTrgmSQL (v13) adds a trigram GIN index backing the library's name-substring search (`name ILIKE '%q%'`), which a btree cannot serve.
const flowsNameTrgmSQL = `
CREATE EXTENSION IF NOT EXISTS pg_trgm;
DROP INDEX IF EXISTS flows_name_trgm_idx;
CREATE INDEX flows_name_trgm_idx ON flows USING gin (name gin_trgm_ops);
`

// flowsNameTrgmDownSQL reverses v13; the pg_trgm extension stays installed.
const flowsNameTrgmDownSQL = `
DROP INDEX IF EXISTS flows_name_trgm_idx;
`

// apiTokenScopesSQL: PAT capability restriction via a comma-joined TEXT column; empty = unscoped = full access.
//
//nolint:gosec // G101 false positive: schema DDL, not a credential
const apiTokenScopesSQL = `
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS scopes TEXT NOT NULL DEFAULT '';
`

//nolint:gosec // G101 false positive: schema DDL, not a credential
const apiTokenScopesDownSQL = `
ALTER TABLE api_tokens DROP COLUMN IF EXISTS scopes;
`

// flowTagsSQL: organizational tagging for the flow library, as a CSV TEXT column of normalized comma-free names; delimiter-anchored LIKE filtering so 'prod' never matches 'production'.
const flowTagsSQL = `
ALTER TABLE flows ADD COLUMN IF NOT EXISTS tags TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS flows_tags_idx ON flows (tags);
`

const flowTagsDownSQL = `
DROP INDEX IF EXISTS flows_tags_idx;
ALTER TABLE flows DROP COLUMN IF EXISTS tags;
`

// govAlertTargetsSQL: target_user_id empty (default) = team-wide; a user ID = visible only to that user.
const govAlertTargetsSQL = `
ALTER TABLE gov_alerts ADD COLUMN IF NOT EXISTS target_user_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS gov_alerts_target_user_idx ON gov_alerts (target_user_id) WHERE target_user_id <> '';
`

const govAlertTargetsDownSQL = `
DROP INDEX IF EXISTS gov_alerts_target_user_idx;
ALTER TABLE gov_alerts DROP COLUMN IF EXISTS target_user_id;
`

// orgChannelsSQL: per-org notification channels; governance events fan out to the org's channels too. RLS mirrors the knowledge tables.
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

ALTER TABLE org_channels ENABLE ROW LEVEL SECURITY;

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

// orgCustomRulesSQL: per-org user-authored analyzer rules (was one deployment-global JSON file); rule_id is the AUTHOR'S id, unique PER ORG. RLS is ENABLE + FORCE in this same migration (ENABLE alone exempts the table OWNER — the app role).
const orgCustomRulesSQL = `
CREATE TABLE IF NOT EXISTS org_custom_rules (
    id         TEXT        PRIMARY KEY,
    org_id     TEXT        NOT NULL,
    rule_id    TEXT        NOT NULL,
    config     JSONB       NOT NULL,
    enabled    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, rule_id)
);
CREATE INDEX IF NOT EXISTS org_custom_rules_org_idx ON org_custom_rules (org_id);

ALTER TABLE org_custom_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_custom_rules FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS rls_org_custom_rules_visible ON org_custom_rules;
CREATE POLICY rls_org_custom_rules_visible ON org_custom_rules FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_custom_rules.org_id AND om.user_id = app_current_user_id())
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_custom_rules.org_id AND om.user_id = app_current_user_id())
);
`

const orgCustomRulesDownSQL = `
DROP TABLE IF EXISTS org_custom_rules;
`

// usageServedModelSQL (D-42): served_model records the model the provider reported answering the call (usage_metrics.model is what the CALLER asked for), empty when unknown or identical. Additive.
const usageServedModelSQL = `
ALTER TABLE usage_metrics ADD COLUMN IF NOT EXISTS served_model TEXT NOT NULL DEFAULT '';
`

const usageServedModelDownSQL = `
ALTER TABLE usage_metrics DROP COLUMN IF EXISTS served_model;
`

// orgProviderKeysSQL stores an organisation's SHARED provider credential, encrypted like provider_keys.
// No RLS deliberately (same reason as provider_keys): every keystore query carries `WHERE org_id = $1`
// and AES-GCM AAD binds each ciphertext to its (org, provider) row. FK cascade removes it with the org.
const orgProviderKeysSQL = `
CREATE TABLE IF NOT EXISTS org_provider_keys (
	org_id     TEXT        NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
	provider   TEXT        NOT NULL,
	ciphertext TEXT        NOT NULL,
	updated_by TEXT        NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (org_id, provider)
);
`

const orgProviderKeysDownSQL = `
DROP TABLE IF EXISTS org_provider_keys;
`

// orgProviderKeysSecretSetBySQL records the org credential's LENDER (the admin who supplied the secret), distinct from updated_by, so the disable-on-departure delete matches the lender; existing rows backfill from updated_by.
//
//nolint:gosec // G101: DDL for the lender column (the name says "secret"), not a credential.
const orgProviderKeysSecretSetBySQL = `
ALTER TABLE org_provider_keys ADD COLUMN IF NOT EXISTS secret_set_by TEXT NOT NULL DEFAULT '';
UPDATE org_provider_keys SET secret_set_by = updated_by WHERE secret_set_by = '';
`

// orgProviderKeysSecretSetByDownSQL reverses v24. Data-lossy only in the lender record; the disable path falls back to updated_by.
//
//nolint:gosec // G101: DDL for the lender column (the name says "secret"), not a credential.
const orgProviderKeysSecretSetByDownSQL = `
ALTER TABLE org_provider_keys DROP COLUMN IF EXISTS secret_set_by;
`

// flowsContentTrgmSQL (R3-5a) backs library content-search pushdown: an index-backed ILIKE over content::text instead of loading every flow in-process; blob-offloaded deployments ("{}" placeholder) report unsupported.
const flowsContentTrgmSQL = `
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX IF NOT EXISTS flows_content_trgm_idx ON flows USING gin ((content::text) gin_trgm_ops);
`

const flowsContentTrgmDownSQL = `
DROP INDEX IF EXISTS flows_content_trgm_idx;
`

// rlsForceSQL closes a silent RLS hole: ENABLE does NOT apply policies to the table's OWNER — only FORCE does — and the app role owns every table (it runs its own migrations); ten tables shipped with ENABLE only, so their policies were inert. Tenant-spanning maintenance paths are unaffected (every policy short-circuits on `NOT app_rls_active()`).
const rlsForceSQL = `
ALTER TABLE flows                 FORCE ROW LEVEL SECURITY;
ALTER TABLE conversations         FORCE ROW LEVEL SECURITY;
ALTER TABLE flow_versions         FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_documents   FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_chunks      FORCE ROW LEVEL SECURITY;
ALTER TABLE flow_analysis_history FORCE ROW LEVEL SECURITY;
ALTER TABLE finding_status        FORCE ROW LEVEL SECURITY;
ALTER TABLE flow_baselines        FORCE ROW LEVEL SECURITY;
ALTER TABLE gov_alerts            FORCE ROW LEVEL SECURITY;
ALTER TABLE org_channels          FORCE ROW LEVEL SECURITY;
`

// rlsForceDownSQL reverses v19, re-opening the owner-exemption hole; round-trip symmetry only, not an operational recommendation.
const rlsForceDownSQL = `
ALTER TABLE flows                 NO FORCE ROW LEVEL SECURITY;
ALTER TABLE conversations         NO FORCE ROW LEVEL SECURITY;
ALTER TABLE flow_versions         NO FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_documents   NO FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_chunks      NO FORCE ROW LEVEL SECURITY;
ALTER TABLE flow_analysis_history NO FORCE ROW LEVEL SECURITY;
ALTER TABLE finding_status        NO FORCE ROW LEVEL SECURITY;
ALTER TABLE flow_baselines        NO FORCE ROW LEVEL SECURITY;
ALTER TABLE gov_alerts            NO FORCE ROW LEVEL SECURITY;
ALTER TABLE org_channels          NO FORCE ROW LEVEL SECURITY;
`

// pgvectorRepairSQL fixes v11, whose single PL/pgSQL block always failed (HNSW needs a fixed dimension; v11's column is dimensionless) and rolled the whole block back — no column, no index, yet v11 recorded itself applied.
// v11 is not edited (its SQL is checksummed); the repair gives each step its OWN block and attempts the index only at a fixed dimension.
const pgvectorRepairSQL = `
DO $$ BEGIN
	CREATE EXTENSION IF NOT EXISTS vector;
EXCEPTION WHEN OTHERS THEN
	RAISE NOTICE 'pgvector extension unavailable; knowledge search stays Go-side';
END $$;

DO $$ BEGIN
	IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
		ALTER TABLE knowledge_chunks ADD COLUMN IF NOT EXISTS embedding_vec vector;
	END IF;
EXCEPTION WHEN OTHERS THEN
	RAISE NOTICE 'pgvector embedding_vec column not added: %', SQLERRM;
END $$;

DO $$ BEGIN
	IF EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'knowledge_chunks' AND column_name = 'embedding_vec'
	) THEN
		UPDATE knowledge_chunks
		SET embedding_vec = ('[' ||
			(SELECT string_agg(e, ',') FROM jsonb_array_elements_text(embedding) AS e)
			|| ']')::vector
		WHERE embedding_vec IS NULL
		  AND jsonb_typeof(embedding) = 'array';
	END IF;
EXCEPTION WHEN OTHERS THEN
	RAISE NOTICE 'pgvector backfill skipped: %', SQLERRM;
END $$;

DO $$
DECLARE dims int;
BEGIN
	SELECT a.atttypmod INTO dims
	  FROM pg_attribute a
	  JOIN pg_class c ON c.oid = a.attrelid
	 WHERE c.relname = 'knowledge_chunks' AND a.attname = 'embedding_vec';

	IF dims IS NULL THEN
		RAISE NOTICE 'pgvector index skipped: embedding_vec column absent';
	ELSIF dims < 1 THEN
		-- Dimensionless by design (mixed-width chunks coexist). HNSW cannot
		-- index it; server-side ordering works regardless.
		RAISE NOTICE 'pgvector index skipped: embedding_vec has no fixed dimension';
	ELSE
		CREATE INDEX IF NOT EXISTS knowledge_chunks_embedding_hnsw
			ON knowledge_chunks USING hnsw (embedding_vec vector_cosine_ops);
	END IF;
EXCEPTION WHEN OTHERS THEN
	RAISE NOTICE 'pgvector index not created: %', SQLERRM;
END $$;
`

// pgvectorRepairDownSQL reverses v20: drop embedding_vec (HNSW goes with it) but never the vector EXTENSION; embedding_vec is derived from the JSONB column, which stays.
const pgvectorRepairDownSQL = `
DO $$ BEGIN
	IF EXISTS (SELECT 1 FROM information_schema.columns
	           WHERE table_name = 'knowledge_chunks' AND column_name = 'embedding_vec') THEN
		ALTER TABLE knowledge_chunks DROP COLUMN embedding_vec;
	END IF;
EXCEPTION WHEN OTHERS THEN
	RAISE NOTICE 'pgvector repair rollback skipped: %', SQLERRM;
END $$;
`

// azureRegistrySQL (v25) is the Azure AI Foundry model registry: resources per owner (twin user/org table sets keyed (owner, id)) and their models.
// Key contracts: no secrets here (credentials stay in provider_keys); never add a user-id FK to org rows (erasure deletes org_members and the policies would hide the rows);
// *_registry_state holds the legacy-import marker; CHECKs mirror interfaces.ValidateAzure*Record; unique-index names map SQLSTATE 23505 to typed errors; RLS ENABLE+FORCE on
// all six plus an explicit owner predicate in every store query (/api/chat/* runs without the RLS transaction).
const azureRegistrySQL = `
CREATE TABLE IF NOT EXISTS user_azure_connections (
	user_id                TEXT        NOT NULL,
	id                     TEXT        NOT NULL,
	name                   TEXT        NOT NULL,
	discovery_dismissed_at TIMESTAMPTZ,
	last_test_ok           BOOLEAN,
	last_test_at           TIMESTAMPTZ,
	last_test_latency_ms   INTEGER,
	last_test_error        TEXT        NOT NULL DEFAULT '',
	created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	CONSTRAINT user_azure_connections_pkey PRIMARY KEY (user_id, id),
	CONSTRAINT user_azure_connections_user_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
	CONSTRAINT user_azure_connections_id_check CHECK (id = 'default' OR id ~ '^[a-z0-9]{10}$'),
	CONSTRAINT user_azure_connections_name_check CHECK (char_length(name) BETWEEN 1 AND 64),
	CONSTRAINT user_azure_connections_last_test_check CHECK (
		(last_test_ok IS NULL) = (last_test_at IS NULL)
		AND (last_test_latency_ms IS NULL OR last_test_latency_ms >= 0)
		AND char_length(last_test_error) <= 500)
);
CREATE UNIQUE INDEX IF NOT EXISTS user_azure_connections_name_idx ON user_azure_connections (user_id, lower(name));

CREATE TABLE IF NOT EXISTS org_azure_connections (
	org_id                 TEXT        NOT NULL,
	id                     TEXT        NOT NULL,
	name                   TEXT        NOT NULL,
	discovery_dismissed_at TIMESTAMPTZ,
	last_test_ok           BOOLEAN,
	last_test_at           TIMESTAMPTZ,
	last_test_latency_ms   INTEGER,
	last_test_error        TEXT        NOT NULL DEFAULT '',
	created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	CONSTRAINT org_azure_connections_pkey PRIMARY KEY (org_id, id),
	CONSTRAINT org_azure_connections_org_fk FOREIGN KEY (org_id) REFERENCES organisations (id) ON DELETE CASCADE,
	CONSTRAINT org_azure_connections_id_check CHECK (id = 'default' OR id ~ '^[a-z0-9]{10}$'),
	CONSTRAINT org_azure_connections_name_check CHECK (char_length(name) BETWEEN 1 AND 64),
	CONSTRAINT org_azure_connections_last_test_check CHECK (
		(last_test_ok IS NULL) = (last_test_at IS NULL)
		AND (last_test_latency_ms IS NULL OR last_test_latency_ms >= 0)
		AND char_length(last_test_error) <= 500)
);
CREATE UNIQUE INDEX IF NOT EXISTS org_azure_connections_name_idx ON org_azure_connections (org_id, lower(name));

CREATE TABLE IF NOT EXISTS user_azure_models (
	user_id              TEXT        NOT NULL,
	id                   TEXT        NOT NULL,
	connection_id        TEXT        NOT NULL,
	kind                 TEXT        NOT NULL,
	deployment           TEXT        NOT NULL,
	display_name         TEXT        NOT NULL,
	purpose              TEXT        NOT NULL DEFAULT 'general',
	purpose_custom       TEXT        NOT NULL DEFAULT '',
	description          TEXT        NOT NULL DEFAULT '',
	context_window       INTEGER,
	max_output_tokens    INTEGER,
	is_kb_embedding      BOOLEAN     NOT NULL DEFAULT FALSE,
	last_test_ok         BOOLEAN,
	last_test_at         TIMESTAMPTZ,
	last_test_latency_ms INTEGER,
	last_test_error      TEXT        NOT NULL DEFAULT '',
	created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	CONSTRAINT user_azure_models_pkey PRIMARY KEY (user_id, id),
	CONSTRAINT user_azure_models_user_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
	CONSTRAINT user_azure_models_connection_fk FOREIGN KEY (user_id, connection_id)
		REFERENCES user_azure_connections (user_id, id) ON DELETE CASCADE,
	CONSTRAINT user_azure_models_id_check CHECK (id ~ '^[a-z0-9]{10}$'),
	CONSTRAINT user_azure_models_kind_check CHECK (kind IN ('chat', 'claude', 'agent', 'embedding')),
	CONSTRAINT user_azure_models_deployment_check CHECK (
		CASE WHEN kind = 'agent'
		     THEN char_length(deployment) BETWEEN 1 AND 128 AND deployment !~ '[\x01-\x1f\x7f]'
		     ELSE deployment ~ '^[A-Za-z0-9._-]{1,64}$'
		END),
	CONSTRAINT user_azure_models_display_name_check CHECK (char_length(display_name) BETWEEN 1 AND 64),
	CONSTRAINT user_azure_models_purpose_check CHECK (
		(purpose IN ('general', 'documentation', 'fixes', 'review', 'refactoring', 'testing') AND purpose_custom = '')
		OR (purpose = 'custom' AND char_length(purpose_custom) BETWEEN 1 AND 32)),
	CONSTRAINT user_azure_models_description_check CHECK (char_length(description) <= 200),
	CONSTRAINT user_azure_models_limits_check CHECK (
		CASE WHEN kind = 'embedding'
		     THEN context_window IS NULL AND max_output_tokens IS NULL
		     ELSE (context_window IS NULL OR context_window BETWEEN 1024 AND 2000000)
		      AND (max_output_tokens IS NULL OR max_output_tokens BETWEEN 256 AND 1000000)
		END),
	CONSTRAINT user_azure_models_kb_check CHECK (NOT is_kb_embedding OR kind = 'embedding'),
	CONSTRAINT user_azure_models_last_test_check CHECK (
		(last_test_ok IS NULL) = (last_test_at IS NULL)
		AND (last_test_latency_ms IS NULL OR last_test_latency_ms >= 0)
		AND char_length(last_test_error) <= 500)
);
CREATE UNIQUE INDEX IF NOT EXISTS user_azure_models_deployment_idx ON user_azure_models
	(user_id, connection_id, (CASE kind WHEN 'claude' THEN 'chat' ELSE kind END), lower(deployment));
CREATE UNIQUE INDEX IF NOT EXISTS user_azure_models_kb_idx ON user_azure_models (user_id) WHERE is_kb_embedding;

CREATE TABLE IF NOT EXISTS org_azure_models (
	org_id               TEXT        NOT NULL,
	id                   TEXT        NOT NULL,
	connection_id        TEXT        NOT NULL,
	kind                 TEXT        NOT NULL,
	deployment           TEXT        NOT NULL,
	display_name         TEXT        NOT NULL,
	purpose              TEXT        NOT NULL DEFAULT 'general',
	purpose_custom       TEXT        NOT NULL DEFAULT '',
	description          TEXT        NOT NULL DEFAULT '',
	context_window       INTEGER,
	max_output_tokens    INTEGER,
	is_kb_embedding      BOOLEAN     NOT NULL DEFAULT FALSE,
	is_default           BOOLEAN     NOT NULL DEFAULT FALSE,
	last_test_ok         BOOLEAN,
	last_test_at         TIMESTAMPTZ,
	last_test_latency_ms INTEGER,
	last_test_error      TEXT        NOT NULL DEFAULT '',
	created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	CONSTRAINT org_azure_models_pkey PRIMARY KEY (org_id, id),
	CONSTRAINT org_azure_models_org_fk FOREIGN KEY (org_id) REFERENCES organisations (id) ON DELETE CASCADE,
	CONSTRAINT org_azure_models_connection_fk FOREIGN KEY (org_id, connection_id)
		REFERENCES org_azure_connections (org_id, id) ON DELETE CASCADE,
	CONSTRAINT org_azure_models_id_check CHECK (id ~ '^[a-z0-9]{10}$'),
	CONSTRAINT org_azure_models_kind_check CHECK (kind IN ('chat', 'claude', 'agent', 'embedding')),
	CONSTRAINT org_azure_models_deployment_check CHECK (
		CASE WHEN kind = 'agent'
		     THEN char_length(deployment) BETWEEN 1 AND 128 AND deployment !~ '[\x01-\x1f\x7f]'
		     ELSE deployment ~ '^[A-Za-z0-9._-]{1,64}$'
		END),
	CONSTRAINT org_azure_models_display_name_check CHECK (char_length(display_name) BETWEEN 1 AND 64),
	CONSTRAINT org_azure_models_purpose_check CHECK (
		(purpose IN ('general', 'documentation', 'fixes', 'review', 'refactoring', 'testing') AND purpose_custom = '')
		OR (purpose = 'custom' AND char_length(purpose_custom) BETWEEN 1 AND 32)),
	CONSTRAINT org_azure_models_description_check CHECK (char_length(description) <= 200),
	CONSTRAINT org_azure_models_limits_check CHECK (
		CASE WHEN kind = 'embedding'
		     THEN context_window IS NULL AND max_output_tokens IS NULL
		     ELSE (context_window IS NULL OR context_window BETWEEN 1024 AND 2000000)
		      AND (max_output_tokens IS NULL OR max_output_tokens BETWEEN 256 AND 1000000)
		END),
	CONSTRAINT org_azure_models_kb_check CHECK (NOT is_kb_embedding OR kind = 'embedding'),
	CONSTRAINT org_azure_models_default_check CHECK (NOT is_default OR kind IN ('chat', 'claude', 'agent')),
	CONSTRAINT org_azure_models_last_test_check CHECK (
		(last_test_ok IS NULL) = (last_test_at IS NULL)
		AND (last_test_latency_ms IS NULL OR last_test_latency_ms >= 0)
		AND char_length(last_test_error) <= 500)
);
CREATE UNIQUE INDEX IF NOT EXISTS org_azure_models_deployment_idx ON org_azure_models
	(org_id, connection_id, (CASE kind WHEN 'claude' THEN 'chat' ELSE kind END), lower(deployment));
CREATE UNIQUE INDEX IF NOT EXISTS org_azure_models_kb_idx ON org_azure_models (org_id) WHERE is_kb_embedding;
CREATE UNIQUE INDEX IF NOT EXISTS org_azure_models_default_idx ON org_azure_models (org_id) WHERE is_default;

CREATE TABLE IF NOT EXISTS user_azure_registry_state (
	user_id     TEXT        NOT NULL,
	imported_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	CONSTRAINT user_azure_registry_state_pkey PRIMARY KEY (user_id),
	CONSTRAINT user_azure_registry_state_user_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS org_azure_registry_state (
	org_id      TEXT        NOT NULL,
	imported_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	CONSTRAINT org_azure_registry_state_pkey PRIMARY KEY (org_id),
	CONSTRAINT org_azure_registry_state_org_fk FOREIGN KEY (org_id) REFERENCES organisations (id) ON DELETE CASCADE
);

ALTER TABLE user_azure_connections    ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_azure_connections    FORCE  ROW LEVEL SECURITY;
ALTER TABLE org_azure_connections     ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_azure_connections     FORCE  ROW LEVEL SECURITY;
ALTER TABLE user_azure_models         ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_azure_models         FORCE  ROW LEVEL SECURITY;
ALTER TABLE org_azure_models          ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_azure_models          FORCE  ROW LEVEL SECURITY;
ALTER TABLE user_azure_registry_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_azure_registry_state FORCE  ROW LEVEL SECURITY;
ALTER TABLE org_azure_registry_state  ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_azure_registry_state  FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS rls_user_azure_connections_visible ON user_azure_connections;
CREATE POLICY rls_user_azure_connections_visible ON user_azure_connections FOR ALL USING (
    NOT app_rls_active() OR user_id = app_current_user_id()
) WITH CHECK (
    NOT app_rls_active() OR user_id = app_current_user_id()
);

DROP POLICY IF EXISTS rls_user_azure_models_visible ON user_azure_models;
CREATE POLICY rls_user_azure_models_visible ON user_azure_models FOR ALL USING (
    NOT app_rls_active() OR user_id = app_current_user_id()
) WITH CHECK (
    NOT app_rls_active() OR user_id = app_current_user_id()
);

DROP POLICY IF EXISTS rls_user_azure_registry_state_visible ON user_azure_registry_state;
CREATE POLICY rls_user_azure_registry_state_visible ON user_azure_registry_state FOR ALL USING (
    NOT app_rls_active() OR user_id = app_current_user_id()
) WITH CHECK (
    NOT app_rls_active() OR user_id = app_current_user_id()
);

DROP POLICY IF EXISTS rls_org_azure_connections_visible ON org_azure_connections;
CREATE POLICY rls_org_azure_connections_visible ON org_azure_connections FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_azure_connections.org_id AND om.user_id = app_current_user_id())
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_azure_connections.org_id AND om.user_id = app_current_user_id())
);

DROP POLICY IF EXISTS rls_org_azure_models_visible ON org_azure_models;
CREATE POLICY rls_org_azure_models_visible ON org_azure_models FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_azure_models.org_id AND om.user_id = app_current_user_id())
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_azure_models.org_id AND om.user_id = app_current_user_id())
);

DROP POLICY IF EXISTS rls_org_azure_registry_state_visible ON org_azure_registry_state;
CREATE POLICY rls_org_azure_registry_state_visible ON org_azure_registry_state FOR ALL USING (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_azure_registry_state.org_id AND om.user_id = app_current_user_id())
) WITH CHECK (
    NOT app_rls_active()
    OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_azure_registry_state.org_id AND om.user_id = app_current_user_id())
);
`

// azureRegistryDownSQL reverses v25. Data-lossy by design: all registry metadata and every NON-default resource's credential (leaving them would orphan credentials whose lender was demoted); starts_with(provider, 'azure-foundry/') matches only those, never the exact "azure-foundry" key.
const azureRegistryDownSQL = `
DROP TABLE IF EXISTS user_azure_models;
DROP TABLE IF EXISTS org_azure_models;
DROP TABLE IF EXISTS user_azure_connections;
DROP TABLE IF EXISTS org_azure_connections;
DROP TABLE IF EXISTS user_azure_registry_state;
DROP TABLE IF EXISTS org_azure_registry_state;

DO $$
DECLARE
	personal integer;
	shared   integer;
BEGIN
	DELETE FROM provider_keys WHERE starts_with(provider, 'azure-foundry/');
	GET DIAGNOSTICS personal = ROW_COUNT;
	DELETE FROM org_provider_keys WHERE starts_with(provider, 'azure-foundry/');
	GET DIAGNOSTICS shared = ROW_COUNT;
	RAISE LOG 'migration v25 down: deleted % personal and % organisation azure-foundry/<id> credentials', personal, shared;
END $$;
`

// perfIndexesV26SQL (v26): flow_collaborators (user_id, flow_id) — the table is keyed (flow_id, user_id), so every lookup BY USER scanned; gov_alerts(flow_id) + unread partial index for the badge; usage_metrics INCLUDE (estimated_cost) covering indexes for an index-only GetDailyUsage; flows_tags_idx dropped — the delimiter-anchored LIKE tags predicate can never use a btree on the CSV.
const perfIndexesV26SQL = `
CREATE INDEX IF NOT EXISTS flow_collaborators_user_flow_idx ON flow_collaborators (user_id, flow_id);

CREATE INDEX IF NOT EXISTS gov_alerts_flow_idx ON gov_alerts (flow_id);
CREATE INDEX IF NOT EXISTS gov_alerts_unread_idx ON gov_alerts (flow_id, target_user_id)
    WHERE read_at IS NULL AND dismissed_at IS NULL;

CREATE INDEX IF NOT EXISTS usage_metrics_user_created_cost_idx ON usage_metrics (user_id, created_at) INCLUDE (estimated_cost);
CREATE INDEX IF NOT EXISTS usage_metrics_org_created_cost_idx ON usage_metrics (org_id, created_at) INCLUDE (estimated_cost);
DROP INDEX IF EXISTS usage_metrics_user_created_idx;
DROP INDEX IF EXISTS usage_metrics_org_created_idx;

DROP INDEX IF EXISTS flows_tags_idx;
`

// perfIndexesV26DownSQL reverses v26: drops the added indexes, recreates the three it dropped. Idempotent.
const perfIndexesV26DownSQL = `
DROP INDEX IF EXISTS flow_collaborators_user_flow_idx;
DROP INDEX IF EXISTS gov_alerts_flow_idx;
DROP INDEX IF EXISTS gov_alerts_unread_idx;

CREATE INDEX IF NOT EXISTS usage_metrics_user_created_idx ON usage_metrics (user_id, created_at);
CREATE INDEX IF NOT EXISTS usage_metrics_org_created_idx ON usage_metrics (org_id, created_at);
DROP INDEX IF EXISTS usage_metrics_user_created_cost_idx;
DROP INDEX IF EXISTS usage_metrics_org_created_cost_idx;

CREATE INDEX IF NOT EXISTS flows_tags_idx ON flows (tags);
`

// perfRound2V27SQL (v27): flow_search, a trigram-indexed side table WITHOUT RLS holding each flow's name/content text — ILIKE/jsonb output are not LEAKPROOF, so the FORCE-RLS flows table's trigram indexes were unusable by the app role; search is `id IN (SELECT … ILIKE …)`, the trigram index serving the subquery while the outer read stays under RLS (trade-off: the ILIKE scans every tenant's text — tenant-correct but a weak cross-tenant existence oracle; maintained in saveFlowTx).
// Also bell/audit/BRIN/knowledge indexes, and the seven flow-child policies rewritten to delegate to rls_flows_visible (row-for-row equivalent, TestPolicyDelegation_V27_MatchesOldVisibility).
const perfRound2V27SQL = `
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE IF NOT EXISTS flow_search (
    flow_id TEXT PRIMARY KEY REFERENCES flows(id) ON DELETE CASCADE,
    name    TEXT NOT NULL DEFAULT '',
    body    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS flow_search_name_trgm_idx ON flow_search USING gin (name gin_trgm_ops);
CREATE INDEX IF NOT EXISTS flow_search_body_trgm_idx ON flow_search USING gin (body gin_trgm_ops);
INSERT INTO flow_search (flow_id, name, body)
    SELECT id, name, content::text FROM flows
    ON CONFLICT (flow_id) DO NOTHING;
DROP INDEX IF EXISTS flows_content_trgm_idx;
DROP INDEX IF EXISTS flows_name_trgm_idx;

CREATE INDEX IF NOT EXISTS gov_alerts_flow_created_idx ON gov_alerts (flow_id, created_at DESC) WHERE dismissed_at IS NULL;
CREATE INDEX IF NOT EXISTS audit_events_resource_created_idx ON audit_events (resource_type, resource_id, created_at DESC);
CREATE INDEX IF NOT EXISTS usage_metrics_created_brin ON usage_metrics USING brin (created_at);
CREATE INDEX IF NOT EXISTS knowledge_documents_org_filename_idx ON knowledge_documents (org_id, filename);

-- Flow-child policies delegate to rls_flows_visible: a flow row is only
-- reachable from inside a policy when the caller may see it, so the child
-- predicate is just "my flow exists". Same names, same FOR clauses.
DROP POLICY IF EXISTS finding_comments_modify ON finding_comments;
CREATE POLICY finding_comments_modify ON finding_comments
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = finding_comments.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = finding_comments.flow_id));

DROP POLICY IF EXISTS rls_conversations_visible ON conversations;
CREATE POLICY rls_conversations_visible ON conversations FOR ALL
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = conversations.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = conversations.flow_id));

DROP POLICY IF EXISTS rls_flow_versions_visible ON flow_versions;
CREATE POLICY rls_flow_versions_visible ON flow_versions FOR ALL
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = flow_versions.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = flow_versions.flow_id));

DROP POLICY IF EXISTS rls_flow_analysis_history_visible ON flow_analysis_history;
CREATE POLICY rls_flow_analysis_history_visible ON flow_analysis_history FOR ALL
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = flow_analysis_history.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = flow_analysis_history.flow_id));

DROP POLICY IF EXISTS rls_finding_status_visible ON finding_status;
CREATE POLICY rls_finding_status_visible ON finding_status FOR ALL
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = finding_status.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = finding_status.flow_id));

DROP POLICY IF EXISTS rls_flow_baselines_visible ON flow_baselines;
CREATE POLICY rls_flow_baselines_visible ON flow_baselines FOR ALL
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = flow_baselines.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = flow_baselines.flow_id));

DROP POLICY IF EXISTS rls_gov_alerts_visible ON gov_alerts;
CREATE POLICY rls_gov_alerts_visible ON gov_alerts FOR ALL
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = gov_alerts.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = gov_alerts.flow_id));
`

// perfRound2V27DownSQL reverses v27: restores the seven flow-child policy bodies, drops the added indexes and the flow_search side table, recreates the v13/v18 trigram indexes; flows data untouched.
const perfRound2V27DownSQL = `
DROP POLICY IF EXISTS finding_comments_modify ON finding_comments;
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

DROP INDEX IF EXISTS knowledge_documents_org_filename_idx;
DROP INDEX IF EXISTS usage_metrics_created_brin;
DROP INDEX IF EXISTS audit_events_resource_created_idx;
DROP INDEX IF EXISTS gov_alerts_flow_created_idx;

DROP TABLE IF EXISTS flow_search;

CREATE INDEX IF NOT EXISTS flows_name_trgm_idx ON flows USING gin (name gin_trgm_ops);
CREATE INDEX IF NOT EXISTS flows_content_trgm_idx ON flows USING gin ((content::text) gin_trgm_ops);
`

// perfRound3V28SQL (v28): an audit_events (action, created_at DESC, id ASC) index serving ListAuditEvents' own order (subsumes the single-column one, dropped), and knowledge_chunks.org_id — denormalised from the parent document (backfilled here, kept filled by a BEFORE INSERT trigger) so rls_knowledge_chunks_visible tests the chunk row alone; admitted rows unchanged (TestPolicy_V28_KnowledgeChunks_VisibilityParity).
const perfRound3V28SQL = `
CREATE INDEX IF NOT EXISTS audit_events_action_created_idx ON audit_events (action, created_at DESC, id ASC);
DROP INDEX IF EXISTS audit_events_action_idx;

ALTER TABLE knowledge_chunks ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
UPDATE knowledge_chunks c SET org_id = d.org_id FROM knowledge_documents d WHERE d.id = c.doc_id AND c.org_id = '';
CREATE INDEX IF NOT EXISTS knowledge_chunks_org_doc_idx ON knowledge_chunks (org_id, doc_id, id);

-- A chunk inserted without an org inherits its document's. Runs as the
-- inserting role: under row-level security the document read sees only
-- documents the caller may see, so a chunk for a foreign document keeps the
-- blank org and the policy's WITH CHECK refuses it, as the baseline did.
CREATE OR REPLACE FUNCTION knowledge_chunks_fill_org() RETURNS trigger
    LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.org_id = '' THEN
        SELECT d.org_id INTO NEW.org_id FROM knowledge_documents d WHERE d.id = NEW.doc_id;
        NEW.org_id := coalesce(NEW.org_id, '');
    END IF;
    RETURN NEW;
END $fn$;
DROP TRIGGER IF EXISTS knowledge_chunks_fill_org ON knowledge_chunks;
CREATE TRIGGER knowledge_chunks_fill_org BEFORE INSERT ON knowledge_chunks
    FOR EACH ROW EXECUTE FUNCTION knowledge_chunks_fill_org();

DROP POLICY IF EXISTS rls_knowledge_chunks_visible ON knowledge_chunks;
CREATE POLICY rls_knowledge_chunks_visible ON knowledge_chunks FOR ALL
    USING (NOT app_rls_active() OR org_id IN (SELECT om.org_id FROM org_members om WHERE om.user_id = app_current_user_id()))
    WITH CHECK (NOT app_rls_active() OR org_id IN (SELECT om.org_id FROM org_members om WHERE om.user_id = app_current_user_id()));
`

// perfRound3V28DownSQL reverses v28: restores the baseline policy (before dropping the column it references), removes trigger, index and column, restores the action index.
const perfRound3V28DownSQL = `
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

DROP TRIGGER IF EXISTS knowledge_chunks_fill_org ON knowledge_chunks;
DROP FUNCTION IF EXISTS knowledge_chunks_fill_org();
DROP INDEX IF EXISTS knowledge_chunks_org_doc_idx;
ALTER TABLE knowledge_chunks DROP COLUMN IF EXISTS org_id;

DROP INDEX IF EXISTS audit_events_action_created_idx;
CREATE INDEX IF NOT EXISTS audit_events_action_idx ON audit_events (action);
`

// perfRound4V29SQL (v29): a flows (lower(name), updated_at DESC, id) index matching the library's name sort (walkable under FORCE RLS), and lz4 TOAST compression for the four wide columns a save touches (a third smaller, ~60 ms vs ~100 ms on a 1.7 MB write; only affects values written from now on; pglz kept via NOTICE without lz4).
const perfRound4V29SQL = `
CREATE INDEX IF NOT EXISTS flows_lower_name_updated_idx ON flows (lower(name), updated_at DESC, id);

DO $v29$ BEGIN
    ALTER TABLE flows ALTER COLUMN content SET COMPRESSION lz4;
    ALTER TABLE flow_search ALTER COLUMN body SET COMPRESSION lz4;
    ALTER TABLE flow_versions ALTER COLUMN content SET COMPRESSION lz4;
    ALTER TABLE conversations ALTER COLUMN messages SET COMPRESSION lz4;
EXCEPTION WHEN feature_not_supported THEN
    RAISE NOTICE 'lz4 toast compression unavailable; keeping pglz';
END $v29$;
`

// perfRound4V29DownSQL reverses v29: drops the index, puts the four columns back on pglz for future values (stored lz4 stays readable).
const perfRound4V29DownSQL = `
DROP INDEX IF EXISTS flows_lower_name_updated_idx;

DO $v29$ BEGIN
    ALTER TABLE flows ALTER COLUMN content SET COMPRESSION pglz;
    ALTER TABLE flow_search ALTER COLUMN body SET COMPRESSION pglz;
    ALTER TABLE flow_versions ALTER COLUMN content SET COMPRESSION pglz;
    ALTER TABLE conversations ALTER COLUMN messages SET COMPRESSION pglz;
EXCEPTION WHEN feature_not_supported THEN
    RAISE NOTICE 'toast compression method change unavailable';
END $v29$;
`
