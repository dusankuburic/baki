-- baki:compat expand
-- Idempotency-Key records of create requests (ADR 0007). The row is inserted in the request's own transaction and
-- its response stored there before the commit, so it commits with the create and vanishes with the create's
-- rollback: a committed row always holds a response. A retry with the key replays it until expires_at; the
-- retention sweep purges expired rows.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    user_id      TEXT        NOT NULL,
    idem_key     TEXT        NOT NULL,
    -- The method and route pattern, and a hash of the path, query and body.
    fingerprint  TEXT        NOT NULL,
    -- 0 while the request runs.
    status       INTEGER     NOT NULL DEFAULT 0,
    content_type TEXT        NOT NULL DEFAULT '',
    -- NULL when the body was over the stored cap. Sealed at rest: a created API token or invite token is in it.
    body         BYTEA,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL,
    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (user_id, idem_key),
    CONSTRAINT idempotency_keys_user_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT idempotency_keys_key_check CHECK (char_length(idem_key) BETWEEN 1 AND 255),
    CONSTRAINT idempotency_keys_status_check CHECK (status = 0 OR status BETWEEN 200 AND 399)
);
CREATE INDEX IF NOT EXISTS idempotency_keys_expires_idx ON idempotency_keys (expires_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS rls_idempotency_keys_visible ON idempotency_keys;
CREATE POLICY rls_idempotency_keys_visible ON idempotency_keys FOR ALL USING (
    NOT app_rls_active() OR user_id = app_current_user_id()
) WITH CHECK (
    NOT app_rls_active() OR user_id = app_current_user_id()
);
