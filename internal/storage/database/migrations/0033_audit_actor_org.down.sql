-- Reverses v33: the index goes with the columns. Data-lossy by design: the organisation, actor kind, token, job
-- and user agent of rows written since are dropped; the rows themselves stay.
DROP INDEX IF EXISTS audit_events_org_created_idx;
ALTER TABLE audit_events
    DROP COLUMN IF EXISTS user_agent,
    DROP COLUMN IF EXISTS actor_job,
    DROP COLUMN IF EXISTS token_id,
    DROP COLUMN IF EXISTS actor_kind,
    DROP COLUMN IF EXISTS org_id;
