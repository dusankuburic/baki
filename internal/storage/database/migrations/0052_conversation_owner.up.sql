-- baki:compat expand
-- Conversations get an owner (R-security-3) and what a server-side thread index lists (R-frontend-c26 (c)).
--
-- owner_id is the user whose turn or save created the row, stamped by every server-side write of this release. NULL
-- is "unowned": every row that exists now, and every row the release before creates during a rolling deploy (it never
-- names the column). Nothing records who wrote such a row (its messages carry no user). Under a client-generated
-- thread id (a uuid none of its messages names as its context block) the key itself is the proof: it is unguessable,
-- so the first write of this release that names an owner (a turn, a save, a title) claims the row for its writer, and
-- whoever presents the key may delete it. Under "flow" or a block id (predictable: every viewer of the flow sees the
-- block ids) the row stays unowned for this release: readable and appendable by the flow's chat audience, its list
-- replaced, named or cleared only by someone who may delete the flow, in nobody's thread index. An owner, once set,
-- never changes (the trigger below refuses it, whoever writes): a row can be claimed, never given away or un-owned.
-- The primary key stays (flow_id, scope) while the release before writes with ON CONFLICT (flow_id, scope): a key holds
-- one conversation, and a key another user owns is refused to the caller (reads see nothing, writes are refused). The
-- contract step one release later keys the table by owner and deals with the unowned rows that remain.
--
-- title is the name the owner gave the thread (POST /api/chat/title, at most 200 characters, clamped by the server);
-- NULL until named. created_at is when the row was created: NULL for the rows that exist now (their creation was not
-- recorded), the column default from now on, inserts of the release before included. message_count is the length of
-- `messages`, kept by the version trigger whenever the messages change, so the thread index never reads the
-- (TOASTed, up to 1 MiB) message list; NULL on a legacy row until its next change.
--
-- Adding nullable columns without a default is metadata only, and SET DEFAULT applies to later inserts only: no
-- table rewrite, no backfill. The index the thread list, the export and erasure read by owner is built after boot,
-- concurrently, by SchemaUpkeep (conversations_owner_idx; the table grows with use).
ALTER TABLE conversations
    ADD COLUMN IF NOT EXISTS owner_id      TEXT,
    ADD COLUMN IF NOT EXISTS title         TEXT,
    ADD COLUMN IF NOT EXISTS created_at    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS message_count INTEGER;
ALTER TABLE conversations ALTER COLUMN created_at SET DEFAULT NOW();

-- v44's trigger function, now also keeping message_count: the version and the count move together, on an insert and
-- on an update that changes the messages, whoever writes (the release before included). And the owner rule: an
-- update may set an unowned row's owner (the claim), never change or clear a set one.
CREATE OR REPLACE FUNCTION conversations_bump_version() RETURNS trigger
    LANGUAGE plpgsql AS $fn$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.owner_id IS NOT NULL AND NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
        RAISE EXCEPTION 'conversation owner cannot change' USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'INSERT' OR NEW.messages IS DISTINCT FROM OLD.messages THEN
        NEW.version := nextval('conversation_version_seq');
        NEW.message_count := CASE WHEN jsonb_typeof(NEW.messages) = 'array' THEN jsonb_array_length(NEW.messages) ELSE 0 END;
    END IF;
    RETURN NEW;
END $fn$;

-- Row-level security: a conversation is its owner's. Reading, creating and changing one needs the flow visible (as
-- since v27) and the row the caller's own or unowned; a row may not be given to someone else, and an update leaves
-- the row the caller's (it claims an unowned one; it can never make one unowned, which would share it). A DELETE also reads
-- through the SELECT policy, so under a user it reaches only that user's and the unowned rows: this release deletes
-- a flow's conversations as the system, after the flow row went under the caller's user (DeleteFlow). The DELETE
-- policy keeps v27's flow-wide rule so that the release before, whose flow deletion runs under the caller's user,
-- still takes the unowned rows and the caller's with the flow (another user's thread, which only this release
-- writes, is left: erasure of its owner takes it). Clearing one thread runs on the chat routes, which run outside
-- the request transaction (no row-level-security user, route.NoTx): the statements there name the owner themselves
-- (postgres_conversations_version.go), and the export and erasure run as the system. The release before reads
-- conversations only there, too, so these policies hide nothing from it. `=` on text and IS NULL are leakproof: the
-- planner may use an index under them.
DROP POLICY IF EXISTS rls_conversations_visible ON conversations;
CREATE POLICY rls_conversations_visible ON conversations FOR SELECT
    USING (NOT app_rls_active() OR (EXISTS (SELECT 1 FROM flows f WHERE f.id = conversations.flow_id)
        AND (conversations.owner_id IS NULL OR conversations.owner_id = app_current_user_id())));
DROP POLICY IF EXISTS rls_conversations_insert ON conversations;
CREATE POLICY rls_conversations_insert ON conversations FOR INSERT
    WITH CHECK (NOT app_rls_active() OR (EXISTS (SELECT 1 FROM flows f WHERE f.id = conversations.flow_id)
        AND (conversations.owner_id IS NULL OR conversations.owner_id = app_current_user_id())));
DROP POLICY IF EXISTS rls_conversations_update ON conversations;
CREATE POLICY rls_conversations_update ON conversations FOR UPDATE
    USING (NOT app_rls_active() OR (EXISTS (SELECT 1 FROM flows f WHERE f.id = conversations.flow_id)
        AND (conversations.owner_id IS NULL OR conversations.owner_id = app_current_user_id())))
    WITH CHECK (NOT app_rls_active() OR (EXISTS (SELECT 1 FROM flows f WHERE f.id = conversations.flow_id)
        AND conversations.owner_id = app_current_user_id()));
DROP POLICY IF EXISTS rls_conversations_delete ON conversations;
CREATE POLICY rls_conversations_delete ON conversations FOR DELETE
    USING (NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = conversations.flow_id));
