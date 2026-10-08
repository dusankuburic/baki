-- baki:compat expand
-- Settings stored per section (ADR 0012): one row per scope and top-level section of the settings document, each
-- with its own version. This release still writes the whole document into app_settings, user_settings and
-- org_settings as well, in the same transaction, so the release before, which reads only those, keeps working after
-- a rollback. legacy_digest is the md5 of the section as written into that blob; when the blob's section no longer
-- matches it, a release that writes only the blob changed the section since, and the blob wins. NULL means the
-- writer did not write the blob (the release that stops writing it). No RLS, like the blob tables: rows are
-- addressed by the caller's own scope id.
CREATE TABLE IF NOT EXISTS settings_sections (
    -- 'app' (the deployment; scope_id ''), 'user' (scope_id = users.id) or 'org' (scope_id = organisations.id).
    scope         TEXT        NOT NULL,
    scope_id      TEXT        NOT NULL,
    -- A top-level key of the settings document: general, appearance, layout, ai, parser, analysis, recentFiles.
    section       TEXT        NOT NULL,
    version       INTEGER     NOT NULL,
    data          JSONB       NOT NULL,
    legacy_digest TEXT,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Derived, for the foreign keys only: a user's and an organisation's sections go with them, as their blob
    -- rows do.
    user_id       TEXT GENERATED ALWAYS AS (CASE WHEN scope = 'user' THEN scope_id END) STORED,
    org_id        TEXT GENERATED ALWAYS AS (CASE WHEN scope = 'org' THEN scope_id END) STORED,
    CONSTRAINT settings_sections_pkey PRIMARY KEY (scope, scope_id, section),
    CONSTRAINT settings_sections_user_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT settings_sections_org_fk FOREIGN KEY (org_id) REFERENCES organisations (id) ON DELETE CASCADE,
    CONSTRAINT settings_sections_scope_check CHECK (scope IN ('user', 'org') OR (scope = 'app' AND scope_id = '')),
    CONSTRAINT settings_sections_section_check CHECK (section <> ''),
    CONSTRAINT settings_sections_version_check CHECK (version >= 1)
);
-- The cascades find a user's or an organisation's rows through these.
CREATE INDEX IF NOT EXISTS settings_sections_user_idx ON settings_sections (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS settings_sections_org_idx ON settings_sections (org_id) WHERE org_id IS NOT NULL;
