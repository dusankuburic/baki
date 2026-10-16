-- baki:compat expand
-- Integrity the schema left to hand-written code (the database-design review, Medium 7 and 8): foreign keys for the
-- organisation-scoped configuration and content, the sessions and the knowledge chunks' organisation; one account per
-- email whatever its case; one invite per token; NOT NULL where the code scans into a string.
--
-- Expand: every constraint accepts what the release before writes. It names an existing organisation for a policy,
-- channel, custom rule or knowledge document (each route checks the caller's membership first; local mode without
-- accounts checks nothing, and its panels send an organisation from the list), an existing user for a session
-- (issued at sign-in and refresh, which load the user first), a chunk's own document and organisation, lowercased
-- emails, random invite tokens, and a string for share_tokens.created_by and policies.description. Left out for that
-- reason: the policy id CHECK (that release stores a policy the client sent without an id under ''; see the README's
-- pending contract steps), and foreign keys on org_members.user_id (local mode on a database has a member, the user
-- 'local', with no users row) and conversations.flow_id (the legacy migrator copies chats about files that were
-- never library flows).
--
-- The step refuses, and changes nothing, when accounts share an email up to case or invites share a token: which one
-- is right is the operator's call. It deletes the policies and custom rules the release before could leave behind
-- (written while their organisation was being deleted) and says how many in the Postgres server log (notices; not the app log). The other rows it could
-- leave (knowledge documents of a deleted organisation, chunks filed under another organisation than their
-- document's, sessions of erased accounts) are in tables that grow with use, which the step neither reads nor
-- row-locks: a request of that release reaches such a row (a re-index of the chunk's organisation, a refresh with
-- the session), and a row lock taken on it before the table's lock below deadlocks with that request. SchemaUpkeep
-- deletes them, counted in the log, before it validates their keys.
--
-- It runs while the release before still serves and holds its locks until it commits. So: the deletes first, under
-- the row locks every write takes, on rows no request or job of that release reaches. Then the tail, in two groups of
-- table locks, each taken all or none (the README's rules): the organisation tables and the three small ones locked
-- against reads too (policies, org_invites, share_tokens), with what touches their rows (the channel orphans, the
-- policy re-key, the NULL fills) after the lock; then refresh_tokens and users, which every sign-in writes, with only
-- their own two statements after them. While it waits for a lock the step holds none of that group's, so no request
-- writing two tables of a group in any order (an erasure, an organisation delete, an invite accept) deadlocks with it;
-- the release before writes refresh_tokens and users in statements of their own, or last. While it waits for those
-- two it still holds the first group (a lock is the transaction's until it commits): a transaction writing users or
-- refresh_tokens for long, which that release has none of, stalls the readers of policies, org_invites and
-- share_tokens for the wait, up to lock_timeout an attempt. A sign-in queued behind the step waits at most for one of the step's waits for
-- those two locks (deadlock_timeout + 200 ms, ~1.2 s, only while an autovacuum holds the table, 200 ms otherwise) and the second group's statements
-- (the users_lower_email_key build: about 2 s at a million accounts). Each constraint is added NOT VALID (no scan: it checks the rows written from now on); the unique
-- indexes it builds are on users, knowledge_documents and org_invites, one row per account, uploaded file and
-- invitation. What reads a table that grows with use runs after boot while writes go on (SchemaUpkeep,
-- postgres_schema_upkeep.go): the VALIDATE of each foreign key and the concurrent build of the indexes the alert
-- inbox's "clear" and account erasure lack (Medium 9 and 10).

DO $v48$
DECLARE
    dups text;
BEGIN
    SELECT string_agg(ids, '; ') INTO dups FROM (
        SELECT string_agg(id, ', ' ORDER BY created_at, id) AS ids FROM users GROUP BY lower(email) HAVING count(*) > 1
    ) d;
    -- The ids only (an email in the log is personal data), and what to do in the message itself: the boot log shows
    -- the message, not a HINT.
    IF dups IS NOT NULL THEN
        RAISE EXCEPTION 'v48 changed nothing: these accounts share an email address up to case (users.id, oldest first): %. Keep one account per address (the recipe: docs/architecture/operations.md, Migration status), then start again.', dups;
    END IF;
    SELECT string_agg(ids, '; ') INTO dups FROM (
        SELECT string_agg(id, ', ' ORDER BY created_at, id) AS ids FROM org_invites GROUP BY token_hash HAVING count(*) > 1
    ) d;
    IF dups IS NOT NULL THEN
        RAISE EXCEPTION 'v48 changed nothing: these organisation invites share a token (org_invites.id, oldest first): %. Delete all but one of each group (the invitee can be invited again), then start again.', dups;
    END IF;
END $v48$;

DO $v48$
DECLARE
    n bigint;
BEGIN
    -- Organisation content written after the organisation-delete steps ran, which then deleted the organisation: rows
    -- no request reaches (the members went with it), in the tables of a row per policy and rule.
    DELETE FROM policies p WHERE NOT EXISTS (SELECT 1 FROM organisations o WHERE o.id = p.org_id);
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n > 0 THEN RAISE WARNING 'v48: deleted % polic(ies) of a deleted organisation', n; END IF;
    DELETE FROM org_custom_rules r WHERE NOT EXISTS (SELECT 1 FROM organisations o WHERE o.id = r.org_id);
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n > 0 THEN RAISE WARNING 'v48: deleted % custom rule(s) of a deleted organisation', n; END IF;
END $v48$;

-- The first group, all or none. A table whose lock is not free is waited for alone, holding none of the others, at
-- most 200 ms at a time with a pause between (a longer wait queues every reader of policies, org_invites or
-- share_tokens behind it, and those requests hold the release before's pool), until the step's lock_timeout has
-- passed: then lock_not_available, and the runner tries the step again. Its children first, then organisations, the
-- parent of four of the keys.
DO $v48$
DECLARE
    tables text[] := ARRAY['knowledge_documents', 'knowledge_chunks', 'org_channels', 'org_custom_rules', 'organisations',
                           'policies', 'org_invites', 'share_tokens'];
    modes  text[] := ARRAY['SHARE ROW EXCLUSIVE', 'SHARE ROW EXCLUSIVE', 'SHARE ROW EXCLUSIVE', 'SHARE ROW EXCLUSIVE', 'SHARE ROW EXCLUSIVE',
                           'ACCESS EXCLUSIVE', 'ACCESS EXCLUSIVE', 'ACCESS EXCLUSIVE'];
    budget   text := current_setting('lock_timeout');
    deadline timestamptz := clock_timestamp() + budget::interval;
    busy     int := 0;
    cur      int;
    waited   boolean;
    slice    int;
    sue      boolean := false;
BEGIN
    LOOP
        waited := false;
        BEGIN
            IF busy > 0 THEN
                waited := true;
                slice := 200;
                IF NOT sue AND EXISTS (SELECT 1 FROM pg_locks l WHERE l.relation = tables[busy]::regclass
                        AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
                        AND l.mode = 'ShareUpdateExclusiveLock' AND l.granted AND l.pid <> pg_backend_pid()) THEN
                    slice := ceil(extract(epoch FROM current_setting('deadlock_timeout')::interval) * 1000)::int + 200;
                    sue := true;
                END IF;
                PERFORM set_config('lock_timeout', CASE WHEN budget::interval = interval '0' THEN slice || 'ms'
                    ELSE greatest(1, least(slice, ceil(extract(epoch FROM deadline - clock_timestamp()) * 1000)))::int || 'ms' END, true);
                EXECUTE format('LOCK TABLE %I IN %s MODE', tables[busy], modes[busy]);
                PERFORM set_config('lock_timeout', budget, true);
                waited := false;
            END IF;
            FOR i IN 1 .. array_length(tables, 1) LOOP
                cur := i;
                CONTINUE WHEN i = busy;
                EXECUTE format('LOCK TABLE %I IN %s MODE NOWAIT', tables[i], modes[i]);
            END LOOP;
            EXIT;
        EXCEPTION WHEN lock_not_available THEN
            -- Every lock this try took is released with it.
            IF budget::interval > interval '0' AND clock_timestamp() >= deadline THEN
                RAISE;
            END IF;
            IF waited THEN
                PERFORM pg_sleep(0.1);
            ELSE
                busy := cur;
                sue := false;
            END IF;
        END;
    END LOOP;
END $v48$;

DO $v48$
DECLARE
    n bigint;
BEGIN
    -- Under the table's lock: the release before's field-encryption backfill rewrites every plaintext channel, these
    -- too, one autocommit UPDATE each.
    DELETE FROM org_channels c WHERE NOT EXISTS (SELECT 1 FROM organisations o WHERE o.id = c.org_id);
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n > 0 THEN RAISE WARNING 'v48: deleted % notification channel(s) of a deleted organisation', n; END IF;
    -- Policies saved without an id: every one the policy panel created, so each organisation's last one. A fresh id
    -- keeps it; this release's service gives every new policy one, so it can be edited again.
    UPDATE policies SET id = gen_random_uuid()::text WHERE id = '';
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n > 0 THEN RAISE WARNING 'v48: gave % polic(ies) saved without an id a generated one', n; END IF;
END $v48$;
-- Both are scanned into a Go string; the release before writes a string to each.
UPDATE policies SET description = '' WHERE description IS NULL;
ALTER TABLE policies ALTER COLUMN description SET NOT NULL,
    ADD CONSTRAINT policies_org_id_fkey FOREIGN KEY (org_id) REFERENCES organisations (id) ON DELETE CASCADE NOT VALID;
UPDATE share_tokens SET created_by = '' WHERE created_by IS NULL;
ALTER TABLE share_tokens ALTER COLUMN created_by SET DEFAULT '', ALTER COLUMN created_by SET NOT NULL;
DROP INDEX IF EXISTS org_invites_token_hash_idx;
CREATE UNIQUE INDEX IF NOT EXISTS org_invites_token_hash_key ON org_invites (token_hash);

-- NOT VALID: the rows already there are checked after boot (SchemaUpkeep, which first deletes the orphans the step
-- left to it).
ALTER TABLE knowledge_documents ADD CONSTRAINT knowledge_documents_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organisations (id) ON DELETE CASCADE NOT VALID;
-- Composite, so a chunk's org_id (denormalised at v28 for the row-level-security policy) can only be its document's.
-- The single-column doc_id foreign key stays; both cascade.
CREATE UNIQUE INDEX IF NOT EXISTS knowledge_documents_id_org_id_key ON knowledge_documents (id, org_id);
ALTER TABLE knowledge_chunks ADD CONSTRAINT knowledge_chunks_doc_id_org_id_fkey
    FOREIGN KEY (doc_id, org_id) REFERENCES knowledge_documents (id, org_id) ON DELETE CASCADE NOT VALID;
ALTER TABLE org_channels ADD CONSTRAINT org_channels_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organisations (id) ON DELETE CASCADE NOT VALID;
ALTER TABLE org_custom_rules ADD CONSTRAINT org_custom_rules_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organisations (id) ON DELETE CASCADE NOT VALID;

-- The second group, last and the same way: what every sign-in writes, so a sign-in queued behind the step's wait
-- waits ≤ deadlock_timeout + 200 ms (~1.2 s) while a vacuum holds the table, 200 ms otherwise (the first group stays held meanwhile: see the header).
DO $v48$
DECLARE
    tables text[] := ARRAY['refresh_tokens', 'users'];
    budget   text := current_setting('lock_timeout');
    deadline timestamptz := clock_timestamp() + budget::interval;
    busy     int := 0;
    cur      int;
    waited   boolean;
    slice    int;
    sue      boolean := false;
BEGIN
    LOOP
        waited := false;
        BEGIN
            IF busy > 0 THEN
                waited := true;
                slice := 200;
                IF NOT sue AND EXISTS (SELECT 1 FROM pg_locks l WHERE l.relation = tables[busy]::regclass
                        AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
                        AND l.mode = 'ShareUpdateExclusiveLock' AND l.granted AND l.pid <> pg_backend_pid()) THEN
                    slice := ceil(extract(epoch FROM current_setting('deadlock_timeout')::interval) * 1000)::int + 200;
                    sue := true;
                END IF;
                PERFORM set_config('lock_timeout', CASE WHEN budget::interval = interval '0' THEN slice || 'ms'
                    ELSE greatest(1, least(slice, ceil(extract(epoch FROM deadline - clock_timestamp()) * 1000)))::int || 'ms' END, true);
                EXECUTE format('LOCK TABLE %I IN SHARE ROW EXCLUSIVE MODE', tables[busy]);
                PERFORM set_config('lock_timeout', budget, true);
                waited := false;
            END IF;
            FOR i IN 1 .. array_length(tables, 1) LOOP
                cur := i;
                CONTINUE WHEN i = busy;
                EXECUTE format('LOCK TABLE %I IN SHARE ROW EXCLUSIVE MODE NOWAIT', tables[i]);
            END LOOP;
            EXIT;
        EXCEPTION WHEN lock_not_available THEN
            IF budget::interval > interval '0' AND clock_timestamp() >= deadline THEN
                RAISE;
            END IF;
            IF waited THEN
                PERFORM pg_sleep(0.1);
            ELSE
                busy := cur;
                sue := false;
            END IF;
        END;
    END LOOP;
END $v48$;

ALTER TABLE refresh_tokens ADD CONSTRAINT refresh_tokens_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE NOT VALID;
-- One account per address. users_email_key stays: the release before and this one look an account up by
-- email = lower(input), which this expression index cannot serve. A case-duplicate the release before registered
-- after the check at the top (users was not locked then) is refused the same way, by its ids.
DO $v48$
DECLARE
    dups text;
BEGIN
    CREATE UNIQUE INDEX IF NOT EXISTS users_lower_email_key ON users (lower(email));
EXCEPTION WHEN unique_violation THEN
    SELECT string_agg(ids, '; ') INTO dups FROM (
        SELECT string_agg(id, ', ' ORDER BY created_at, id) AS ids FROM users GROUP BY lower(email) HAVING count(*) > 1
    ) d;
    RAISE EXCEPTION 'v48 changed nothing: these accounts share an email address up to case (users.id, oldest first): %. Keep one account per address (the recipe: docs/architecture/operations.md, Migration status), then start again.', dups;
END $v48$;
