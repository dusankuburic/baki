-- baki:compat expand
-- The CI webhook's replay record (ADR 0019). POST /api/integrations/ci inserts (caller, delivery_id) once the call
-- has authenticated and before it runs the analysis; a pair already present is a replay, answered 409 CI_REPLAYED.
-- caller names the key that authenticated the call, never a person: "credential:<key id>" (an organisation CI
-- credential), "secret" (the deployment's PAD_CI_WEBHOOK_SECRET) or "token:<API token id>" (a personal token with
-- the ci scope). A record is kept an hour, long after a signature's five-minute window has closed: each insert
-- removes its caller's older records, and the retention sweep the rest. A new table only: the release before neither
-- reads nor writes it, and ignores the deliveryId field.
CREATE TABLE IF NOT EXISTS ci_webhook_deliveries (
    caller      TEXT        NOT NULL,
    delivery_id TEXT        NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ci_webhook_deliveries_pkey PRIMARY KEY (caller, delivery_id),
    CONSTRAINT ci_webhook_deliveries_id_check CHECK (char_length(delivery_id) BETWEEN 16 AND 128)
);
-- The insert's own clean-up, by caller; the retention sweep's, across callers.
CREATE INDEX IF NOT EXISTS ci_webhook_deliveries_caller_received_idx ON ci_webhook_deliveries (caller, received_at);
CREATE INDEX IF NOT EXISTS ci_webhook_deliveries_received_idx ON ci_webhook_deliveries (received_at);

-- Only the pool sees the table: the webhook writes it before any user is known, and the sweep purges it. Inside a
-- user's request transaction no row is visible or writable.
ALTER TABLE ci_webhook_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE ci_webhook_deliveries FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_ci_webhook_deliveries_pool ON ci_webhook_deliveries;
CREATE POLICY rls_ci_webhook_deliveries_pool ON ci_webhook_deliveries FOR ALL
    USING (NOT app_rls_active()) WITH CHECK (NOT app_rls_active());
