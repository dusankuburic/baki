-- Reverses v37. Data-lossy by design: the recorded rule sets and the notices are dropped; baselines and health
-- snapshots stay and read as legacy records.
DROP TABLE IF EXISTS rule_notices;
DROP INDEX IF EXISTS flow_analysis_rule_set_digest_idx;
DROP INDEX IF EXISTS flow_baselines_rule_set_digest_idx;
ALTER TABLE flow_analysis
    DROP COLUMN IF EXISTS rule_set_at,
    DROP COLUMN IF EXISTS rule_set_digest,
    DROP COLUMN IF EXISTS rule_set;
ALTER TABLE flow_baselines
    DROP COLUMN IF EXISTS rule_set_at,
    DROP COLUMN IF EXISTS rule_set_digest,
    DROP COLUMN IF EXISTS rule_set;
