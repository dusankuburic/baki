-- Reverses v46. The legacy data migration then reports its status from the replica that ran it, as before.
ALTER TABLE job_runs DROP COLUMN IF EXISTS last_detail;
