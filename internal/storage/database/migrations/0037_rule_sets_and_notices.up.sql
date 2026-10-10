-- baki:compat expand
-- Rule sets on baselines and health snapshots, and the "new rules" notices (ADR 0016). All additive. A baseline and a
-- health snapshot record the rule set of the analysis they were taken from (models.RuleSet), so the governance
-- scanner can tell the findings of rules newer than the record from drift. NULL is unknown and reads as a legacy
-- record. rule_set_at is written in the same statement as created_at (analyzed_at) and with the same value, and a
-- rule set counts only while the two are equal: the release before writes these tables with explicit column lists,
-- so a row it rewrites gets a new created_at (analyzed_at), keeps the old rule_set_at and reads as legacy again.
ALTER TABLE flow_baselines
    ADD COLUMN IF NOT EXISTS rule_set        JSONB,
    ADD COLUMN IF NOT EXISTS rule_set_digest TEXT,
    ADD COLUMN IF NOT EXISTS rule_set_at     TIMESTAMPTZ;
ALTER TABLE flow_analysis
    ADD COLUMN IF NOT EXISTS rule_set        JSONB,
    ADD COLUMN IF NOT EXISTS rule_set_digest TEXT,
    ADD COLUMN IF NOT EXISTS rule_set_at     TIMESTAMPTZ;
-- The scanner resolves the digests of a page (RuleSetsByDigest) through these; every row before this step is NULL.
CREATE INDEX IF NOT EXISTS flow_baselines_rule_set_digest_idx ON flow_baselines (rule_set_digest)
    WHERE rule_set_digest IS NOT NULL;
CREATE INDEX IF NOT EXISTS flow_analysis_rule_set_digest_idx ON flow_analysis (rule_set_digest)
    WHERE rule_set_digest IS NOT NULL;

-- One notice per scope and rule set: the rules newer than the scope's baselines or snapshots that report findings,
-- announced once (the primary key decides) instead of a drift or regression alert per flow. Read and dismissed stamps
-- are scope-wide, like gov_alerts'. A user's and an organisation's notices go with them through the foreign keys on
-- the generated columns, also under the release before, which knows nothing of this table.
CREATE TABLE IF NOT EXISTS rule_notices (
    -- kind || '|' || scope || '|' || scope_id || '|' || rule_set_digest
    id              TEXT        PRIMARY KEY,
    kind            TEXT        NOT NULL,
    -- 'org' (scope_id = organisations.id), 'user' (scope_id = users.id) or 'app' (a deployment without accounts).
    scope           TEXT        NOT NULL,
    scope_id        TEXT        NOT NULL,
    rule_set_digest TEXT        NOT NULL,
    -- The sorted, distinct rule ids announced so far; an upsert adds, never removes.
    rule_ids        JSONB       NOT NULL DEFAULT '[]',
    flow_count      INTEGER     NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    read_at         TIMESTAMPTZ,
    dismissed_at    TIMESTAMPTZ,
    user_id         TEXT GENERATED ALWAYS AS (CASE WHEN scope = 'user' THEN scope_id END) STORED,
    org_id          TEXT GENERATED ALWAYS AS (CASE WHEN scope = 'org' THEN scope_id END) STORED,
    CONSTRAINT rule_notices_user_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT rule_notices_org_fk FOREIGN KEY (org_id) REFERENCES organisations (id) ON DELETE CASCADE,
    CONSTRAINT rule_notices_scope_check CHECK (scope IN ('user', 'org') OR (scope = 'app' AND scope_id = '')),
    CONSTRAINT rule_notices_kind_check CHECK (kind <> '')
);
-- The cascades find a user's or an organisation's rows through these; the retention sweep reads updated_at.
CREATE INDEX IF NOT EXISTS rule_notices_user_idx ON rule_notices (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS rule_notices_org_idx ON rule_notices (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS rule_notices_updated_idx ON rule_notices (updated_at);

-- An org notice is its members', a user notice that user's; an app notice is reachable only outside a user's scope
-- (local mode, the scanner). The application repeats this predicate (ruleNoticeVisible): RLS is inert under a
-- BYPASSRLS role.
ALTER TABLE rule_notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE rule_notices FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_rule_notices_visible ON rule_notices;
CREATE POLICY rls_rule_notices_visible ON rule_notices FOR ALL
    USING (NOT app_rls_active()
           OR (scope = 'user' AND scope_id = app_current_user_id())
           OR (scope = 'org' AND EXISTS (SELECT 1 FROM org_members om
                                          WHERE om.org_id = rule_notices.scope_id AND om.user_id = app_current_user_id())))
    WITH CHECK (NOT app_rls_active()
           OR (scope = 'user' AND scope_id = app_current_user_id())
           OR (scope = 'org' AND EXISTS (SELECT 1 FROM org_members om
                                          WHERE om.org_id = rule_notices.scope_id AND om.user_id = app_current_user_id())));
