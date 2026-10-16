-- Reverses v47. The snapshots stay; only the per-user bound's column and index go, and the release before trims per
-- flow only.
DROP INDEX IF EXISTS flow_snapshots_user_seq_idx;
ALTER TABLE flow_snapshots DROP COLUMN IF EXISTS user_id;
