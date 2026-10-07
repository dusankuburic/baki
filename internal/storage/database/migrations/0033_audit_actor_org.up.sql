-- baki:compat expand
-- The audit trail records the organisation an action belongs to and who acted (ADR 0008). All columns are
-- nullable and NULL on the rows written before: an older binary keeps inserting its nine columns. org_id of older
-- rows is filled by a resumable boot-time backfill (BackfillAuditOrgIDs), never in this step. audit_events has no
-- RLS policy (only the admin endpoints and the data-subject paths read it), and these columns do not add one.
ALTER TABLE audit_events
    ADD COLUMN IF NOT EXISTS org_id     TEXT,
    -- auth.ActorKind: user, token or system.
    ADD COLUMN IF NOT EXISTS actor_kind TEXT,
    -- The API token a token actor used.
    ADD COLUMN IF NOT EXISTS token_id   TEXT,
    -- The background job a system actor ran.
    ADD COLUMN IF NOT EXISTS actor_job  TEXT,
    ADD COLUMN IF NOT EXISTS user_agent TEXT;

-- Listing an organisation's trail newest first, in ListAuditEvents' order. Partial: rows without an
-- organisation (most of the table at first) are not indexed, so the build reads the table without writing it.
CREATE INDEX IF NOT EXISTS audit_events_org_created_idx
    ON audit_events (org_id, created_at DESC, id ASC) WHERE org_id IS NOT NULL;
