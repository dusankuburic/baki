-- Reverses v32: the table goes with its policy and index. Data-lossy by design: retries of creates made before the
-- rollback run again instead of replaying.
DROP TABLE IF EXISTS idempotency_keys;
