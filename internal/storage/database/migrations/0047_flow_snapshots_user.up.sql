-- baki:compat expand
-- The per-user bound of the undo ring (Phase 5b close-out): a push also trims the pushing user's snapshots across
-- every flow, so a user editing many flows cannot multiply the per-flow bound (20 per flow, 32 MiB). user_id is who
-- made the mutation a snapshot precedes, '' for none. A constant default keeps the ADD COLUMN a catalogue change, and
-- the release before, which lists its insert columns and never reads this one, writes '' (not counted by the bound).
-- Erasure clears the user's id on the rows that stay with other people's flows (lifecycle_flows.go).
ALTER TABLE flow_snapshots ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
-- The trim's walk: one user's rows newest first. Built inside the step: the table only holds the last 24 hours of
-- snapshots (the retention sweep deletes older rows), so the build, and the write lock it takes, are short.
CREATE INDEX IF NOT EXISTS flow_snapshots_user_seq_idx ON flow_snapshots (user_id, seq DESC) WHERE user_id <> '';
