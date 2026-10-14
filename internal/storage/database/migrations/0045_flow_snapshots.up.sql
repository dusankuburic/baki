-- baki:compat expand
-- The cloud undo ring (Phase 5b): the pre-mutation source a fix, a batch fix, a source save or a restore replaced,
-- so an undo taken on one replica finds what another recorded, and a restart or deploy no longer drops it. A new
-- table only: the release before keeps its in-memory ring and neither reads nor writes this one. Bounded by the
-- store, not by the schema: the push keeps the newest 20 per flow within a byte bound, reads hide rows older than
-- 24 hours and the retention sweep deletes them. No user id is stored: the text is the flow's content, which goes
-- with the flow (export, erasure and organisation deletion are classified in lifecycle_flows.go).
CREATE TABLE IF NOT EXISTS flow_snapshots (
    id             TEXT        PRIMARY KEY,
    flow_id        TEXT        NOT NULL REFERENCES flows (id) ON DELETE CASCADE,
    -- seq orders a flow's ring (push order, newest last); created_at is the 24 hour clock.
    seq            BIGSERIAL   NOT NULL,
    label          TEXT        NOT NULL DEFAULT '',
    -- bytes is the display size (the member texts); retained_bytes what the per-flow bound counts (names too).
    bytes          BIGINT      NOT NULL DEFAULT 0,
    retained_bytes BIGINT      NOT NULL DEFAULT 0,
    -- source is a single-file flow's text; files a folder flow's per-file texts (name -> text), NULL otherwise.
    source         TEXT        NOT NULL DEFAULT '',
    files          JSONB,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT flow_snapshots_id_check CHECK (char_length(id) BETWEEN 1 AND 64)
);
CREATE INDEX IF NOT EXISTS flow_snapshots_flow_seq_idx ON flow_snapshots (flow_id, seq DESC);
-- The retention sweep's condition, walked in batches.
CREATE INDEX IF NOT EXISTS flow_snapshots_created_at_idx ON flow_snapshots (created_at);

DO $v45$ BEGIN
    ALTER TABLE flow_snapshots ALTER COLUMN source SET COMPRESSION lz4;
    ALTER TABLE flow_snapshots ALTER COLUMN files  SET COMPRESSION lz4;
EXCEPTION WHEN feature_not_supported THEN
    RAISE NOTICE 'lz4 toast compression unavailable; keeping pglz';
END $v45$;

-- Visible to whoever may open the flow, like the other flow-child tables (v34): the policy asks flows, whose own
-- policy admits the owner, a collaborator and an organisation member. Without a user context (the retention sweep,
-- local mode) every row is visible, as everywhere.
ALTER TABLE flow_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE flow_snapshots FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_flow_snapshots_visible ON flow_snapshots;
CREATE POLICY rls_flow_snapshots_visible ON flow_snapshots FOR ALL
    USING      (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = flow_snapshots.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = flow_snapshots.flow_id));
