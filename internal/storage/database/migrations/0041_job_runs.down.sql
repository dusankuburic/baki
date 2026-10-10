-- Reverses v41. The jobs' run state goes, and with it the cross-replica overdue signal; the release before never
-- reads the table.
DROP TABLE IF EXISTS job_runs;
