-- Reverses v52: v27's single flow-wide policy comes back, the trigger function loses the count (v44's body), and
-- the columns go, with the index SchemaUpkeep builds over them after boot. The release before never reads the
-- columns. Owners, titles and creation times are lost; the conversations themselves stay.
DROP POLICY IF EXISTS rls_conversations_delete ON conversations;
DROP POLICY IF EXISTS rls_conversations_update ON conversations;
DROP POLICY IF EXISTS rls_conversations_insert ON conversations;
DROP POLICY IF EXISTS rls_conversations_visible ON conversations;
CREATE POLICY rls_conversations_visible ON conversations FOR ALL
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = conversations.flow_id))
    WITH CHECK (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = conversations.flow_id));

CREATE OR REPLACE FUNCTION conversations_bump_version() RETURNS trigger
    LANGUAGE plpgsql AS $fn$
BEGIN
    IF TG_OP = 'INSERT' OR NEW.messages IS DISTINCT FROM OLD.messages THEN
        NEW.version := nextval('conversation_version_seq');
    END IF;
    RETURN NEW;
END $fn$;

DROP INDEX IF EXISTS conversations_owner_idx;
ALTER TABLE conversations
    DROP COLUMN IF EXISTS message_count,
    DROP COLUMN IF EXISTS created_at,
    DROP COLUMN IF EXISTS title,
    DROP COLUMN IF EXISTS owner_id;
