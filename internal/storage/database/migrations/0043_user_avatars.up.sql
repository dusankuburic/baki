-- baki:compat expand
-- Uploaded profile avatars: one re-encoded image per user. Stored as bytea because every mode already has this
-- store and a replica (Phase 5b) needs no shared volume to serve it. A new table only: the release before never
-- reads it, and users.avatar_url keeps pointing wherever it did.
CREATE TABLE IF NOT EXISTS user_avatars (
    user_id    TEXT        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    -- 'image/png' or 'image/jpeg' — the only formats the upload path re-encodes into.
    mime_type  TEXT        NOT NULL,
    bytes      BYTEA       NOT NULL,
    byte_size  INTEGER     NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Unlike users (no RLS: its reads are Go-layer authorised), an avatar is served to any authenticated caller —
-- comment authors, the admin list — but written only by its owner; the handlers take the user id from the session
-- claims, never from the request. The write path runs on the pool like every user write, where app_rls_active() is
-- false and the policies short-circuit to allow-all as they do everywhere.
ALTER TABLE user_avatars ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_avatars FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_user_avatars_read ON user_avatars;
CREATE POLICY rls_user_avatars_read ON user_avatars FOR SELECT
    USING (NOT app_rls_active() OR app_current_user_id() <> '');
DROP POLICY IF EXISTS rls_user_avatars_own ON user_avatars;
CREATE POLICY rls_user_avatars_own ON user_avatars FOR ALL
    USING      (NOT app_rls_active() OR user_id = app_current_user_id())
    WITH CHECK (NOT app_rls_active() OR user_id = app_current_user_id());
