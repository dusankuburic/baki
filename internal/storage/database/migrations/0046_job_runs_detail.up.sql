-- baki:compat expand
-- The detail a job leaves for the replicas that did not run it (ADR 0023): the legacy data migration's result, which
-- GET /api/admin/migration/status reads from any replica. A nullable column nothing reads or writes before this
-- release, so the release before runs on it unchanged.
ALTER TABLE job_runs ADD COLUMN IF NOT EXISTS last_detail JSONB;
