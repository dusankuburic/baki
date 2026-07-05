package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

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
}

// migrations is the ordered, append-only set of schema versions. Version 1 is
// the baseline: the entire original idempotent schema, recorded so existing
// deployments stop re-running it every boot and so future steps build on a
// known version. Append new versions only at the end; never edit or reorder an
// already-shipped step.
var migrations = []migration{
	{version: 1, name: "baseline", sql: schemaBaseline},
	{version: 2, name: "comments_and_shares", sql: commentsAndSharesSQL},
}

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
	_, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER    PRIMARY KEY,
		name       TEXT       NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	return err
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	return tx.Commit()
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
	metadata    JSONB       NOT NULL DEFAULT '{}',
	owner_id    TEXT        NOT NULL DEFAULT '',
	org_id      TEXT        NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	version     INTEGER     NOT NULL DEFAULT 0
);
-- OCC version column (added post-1.0; ALTER for existing deployments).
ALTER TABLE flows ADD COLUMN IF NOT EXISTS version INTEGER NOT NULL DEFAULT 0;
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
               OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = app_current_user_id())
               OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = app_current_user_id()))
    )
);
`
