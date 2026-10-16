-- baki:compat expand
-- Indexes for the reads that walked whole tables, and the removal of indexes nothing needs (the database-design
-- review, Medium 13, 15 and 16).
--
-- Expand: every index here is non-unique and named by no ON CONFLICT, so the release before reads and writes exactly
-- as it did. The ones dropped served it no better than what replaces them (TestMigration0049_* checks its reads still
-- reach each table through an index): at most a slower plan, never another result.
--
-- The step runs while the release before still serves, and holds each lock from when it is granted until it commits:
-- a build's SHARE (writes to the table wait) and a drop's ACCESS EXCLUSIVE (reads wait too, and while a drop waits for
-- its lock every later query on the table queues behind it). So the step only builds, on two small tables written
-- rarely: knowledge_documents (a row per uploaded file) and rule_notices (a row per scope and rule set, written by the
-- scanner and reached by an organisation delete's cascade after its documents), taking both locks first, all or none
-- as 0048 does: holding one while it waited for the other made uploads wait for the scanner's page, or deadlocked with
-- an organisation delete. The indexes of users, which every sign-in writes, are built concurrently after boot, and
-- the four drops are concurrent after boot too (SchemaUpkeep, postgres_schema_upkeep.go), once the replacement below
-- exists: no write waits for them, and they never wait for a lock while holding another.
--
-- users_role_idx (v1) carries a wrong comment in its shipped step ("Supports the per-row collaborator EXISTS subquery
-- in ListFlows and ListCollaborators"): it indexes users (role), for ListAdmins and the role filter of the admin user
-- list. The index is right; only the comment is not, and a shipped step is never edited.

-- A table whose lock is not free is waited for alone, at most deadlock_timeout + 200 ms (~1.2 s) while a vacuum holds the table,
-- 200 ms otherwise, with a pause between, until the step's lock_timeout has passed: then lock_not_available, and the runner tries the step again.
DO $v49$
DECLARE
    tables text[] := ARRAY['knowledge_documents', 'rule_notices'];
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
                EXECUTE format('LOCK TABLE %I IN SHARE MODE', tables[busy]);
                PERFORM set_config('lock_timeout', budget, true);
                waited := false;
            END IF;
            FOR i IN 1 .. array_length(tables, 1) LOOP
                cur := i;
                CONTINUE WHEN i = busy;
                EXECUTE format('LOCK TABLE %I IN SHARE MODE NOWAIT', tables[i]);
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
END $v49$;

-- ListKnowledgeDocuments pages an organisation's documents newest first (created_at DESC, id): this serves the page
-- in its order, where the (org_id) index it replaces found the rows and sorted them. It serves every org_id lookup the
-- old one did, the foreign key's cascade from organisations included; so does knowledge_documents_org_filename_idx.
CREATE INDEX IF NOT EXISTS knowledge_documents_org_created_idx ON knowledge_documents (org_id, created_at DESC, id);

-- The rule-notice reads name the generated user_id and org_id columns (rule_notices_user_idx, rule_notices_org_idx);
-- this is the third arm of their BitmapOr, the app notices every caller sees. Written only in a deployment without
-- accounts, so empty in every other one.
CREATE INDEX IF NOT EXISTS rule_notices_app_idx ON rule_notices (id) WHERE scope = 'app';
