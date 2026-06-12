package database

import (
	"context"
	"fmt"
)

// ---- Schema migration ----

// migrationLockKey is an arbitrary fixed 64-bit integer used as the key for
// pg_advisory_lock during schema migration. Any value works as long as it is
// unique to this application — pick something derived from the project name
// so it does not collide with locks taken by other software sharing the DB.
const migrationLockKey int64 = 0x70616461_6E616C7A // "padanalz" in hex bytes

// migrate runs the embedded schema, serializing concurrent startups across
// replicas using a PostgreSQL session-level advisory lock. Without the lock,
// two pods starting simultaneously can both execute the DO $$ ... $$ migration
// blocks (e.g. provider_keys PK rebuild) and one of them will fail with a
// duplicate-object or constraint-violation error.
//
// pg_advisory_lock is automatically released when the session ends, so we
// dedicate a single connection to the migration and release the lock
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

	if _, err := conn.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("execute schema: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id          TEXT        PRIMARY KEY,
	email       TEXT        UNIQUE NOT NULL,
	password    TEXT        NOT NULL,
	role        TEXT        NOT NULL,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Profile fields (added post-1.0; ALTER for existing deployments).
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
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
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
`
