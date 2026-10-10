-- baki:compat expand
-- Organisation CI credentials (ADR 0019): server-minted HMAC keys an org admin hands to a CI runner, which signs
-- POST /api/integrations/ci with one to analyse that organisation's flows. A new table only; the release before
-- never reads it, and its webhook keeps answering the deployment-wide PAD_CI_WEBHOOK_SECRET as it always has.
CREATE TABLE IF NOT EXISTS org_ci_credentials (
    -- The public key id (X-Baki-Key-Id), part of the credential string baki_ci_<id>_<secret>.
    id           TEXT        PRIMARY KEY,
    org_id       TEXT        NOT NULL REFERENCES organisations (id) ON DELETE CASCADE,
    name         TEXT        NOT NULL DEFAULT '',
    -- The full credential string, the HMAC key, sealed with the field encryption key ("enc:v1:", field_crypto.go).
    -- The store refuses to write one without that key, so no row holds it in plaintext.
    secret       TEXT        NOT NULL,
    -- The credential's last four characters, for the admin list.
    secret_hint  TEXT        NOT NULL DEFAULT '',
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- NULL: never expires.
    expires_at   TIMESTAMPTZ,
    -- Throttled to one write per five minutes.
    last_used_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS org_ci_credentials_org_idx ON org_ci_credentials (org_id);

-- An organisation's members see its credentials, as they see its channels (rls_org_channels_visible); the routes
-- add the org-admin check. The webhook resolves a key id on the pool, before any user is known.
ALTER TABLE org_ci_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_ci_credentials FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_org_ci_credentials_visible ON org_ci_credentials;
CREATE POLICY rls_org_ci_credentials_visible ON org_ci_credentials FOR ALL
    USING      (NOT app_rls_active() OR EXISTS (SELECT 1 FROM org_members om
                                                 WHERE om.org_id = org_ci_credentials.org_id AND om.user_id = app_current_user_id()))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM org_members om
                                                 WHERE om.org_id = org_ci_credentials.org_id AND om.user_id = app_current_user_id()));
