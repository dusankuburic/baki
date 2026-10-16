-- Reverses v49: the new indexes go, the users indexes SchemaUpkeep builds after boot on a v49 schema included, and
-- the dropped ones come back. The HNSW index v49 dropped never existed, so nothing restores it. A retired index an
-- interrupted concurrent drop left invalid (still written on every insert, never read) is dropped first: IF NOT EXISTS
-- would keep it. audit_events_org_created_idx is built over the whole audit log, writes to it waiting for the build:
-- last, so a lock wait that times out (and the retry) never throws a finished build away.
DROP INDEX IF EXISTS rule_notices_app_idx;
DROP INDEX IF EXISTS users_locked_until_idx;
DROP INDEX IF EXISTS users_display_name_trgm_idx;
DROP INDEX IF EXISTS users_email_trgm_idx;
DROP INDEX IF EXISTS knowledge_documents_org_created_idx;
DO $v49$
DECLARE
    ix text;
BEGIN
    FOREACH ix IN ARRAY ARRAY['knowledge_docs_org_idx', 'org_custom_rules_org_idx', 'audit_events_org_created_idx'] LOOP
        IF EXISTS (SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
                    WHERE c.relname = ix AND c.relnamespace = current_schema()::regnamespace AND NOT i.indisvalid) THEN
            EXECUTE format('DROP INDEX %I', ix);
        END IF;
    END LOOP;
END $v49$;
CREATE INDEX IF NOT EXISTS knowledge_docs_org_idx ON knowledge_documents (org_id);
CREATE INDEX IF NOT EXISTS org_custom_rules_org_idx ON org_custom_rules (org_id);
CREATE INDEX IF NOT EXISTS audit_events_org_created_idx ON audit_events (org_id, created_at DESC, id ASC) WHERE org_id IS NOT NULL;
