-- baki:compat expand
-- The lexical half of hybrid knowledge retrieval (R-knowledge-8): a question quoting an exact PAD action name, a
-- %Variable%, an error code or a file name reaches the chunk that names it even when the embedding ranks that chunk
-- under the relevance floor. The search finds the chunks whose text holds a phrase of the question
-- (content %> phrase: pg_trgm word similarity at or above the floor), which a trigram index serves.
--
-- Not on knowledge_chunks itself: that table is FORCE ROW LEVEL SECURITY, and %> is not LEAKPROOF, so the planner
-- never uses an index for it ahead of the row policy, for the app role or anyone else it applies to (v27 found the
-- same of flows' name and content and moved their trigram indexes to flow_search). knowledge_chunk_search is the
-- side table the index lives on, WITHOUT row-level security, one row per chunk: the search asks it for the ids of
-- the org's matching chunks and reads the chunks themselves under their policy, so what it returns is exactly what
-- the vector search may return. The trade-off is flow_search's: the probe reads every tenant's index entries, a weak
-- cross-tenant timing oracle, never a row.
--
-- A trigger keeps it: every chunk inserted, or whose text or organisation changes, is written to it; a chunk's
-- delete cascades. The chunks stored before this step are copied after boot, in pages (SchemaUpkeep,
-- postgres_schema_upkeep.go): until then the lexical search does not see them, and the vector search is unchanged.
--
-- Expand: a new table, a function and a trigger. The release before writes knowledge_chunks as it did and the
-- trigger fills the side table for it; it never reads the table. The step takes SHARE ROW EXCLUSIVE on
-- knowledge_chunks (the trigger and the foreign key need it) for a few catalogue writes, first, so its two statements
-- never wait for it in turn; uploads wait for the step, and the step for an upload's commit, within its lock timeout.
LOCK TABLE knowledge_chunks IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE IF NOT EXISTS knowledge_chunk_search (
    chunk_id TEXT PRIMARY KEY REFERENCES knowledge_chunks(id) ON DELETE CASCADE,
    org_id   TEXT NOT NULL,
    content  TEXT NOT NULL
);
-- One GIN over both columns, so a probe for one org's matches of one phrase is one index scan: pg_trgm answers the
-- org's equality (since PostgreSQL 14) and the phrase's %> together. With a separate org index the planner reads the
-- whole org and tests every chunk's similarity, which costs far more than its estimate says. Empty at creation, so
-- built here; the copy after boot fills it.
CREATE INDEX IF NOT EXISTS knowledge_chunk_search_trgm_idx ON knowledge_chunk_search USING gin (org_id gin_trgm_ops, content gin_trgm_ops);

CREATE OR REPLACE FUNCTION knowledge_chunk_search_sync() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    INSERT INTO knowledge_chunk_search (chunk_id, org_id, content)
    VALUES (NEW.id, NEW.org_id, NEW.content)
    ON CONFLICT (chunk_id) DO UPDATE SET org_id = EXCLUDED.org_id, content = EXCLUDED.content;
    RETURN NULL;
END
$fn$;

DROP TRIGGER IF EXISTS knowledge_chunk_search_sync ON knowledge_chunks;
CREATE TRIGGER knowledge_chunk_search_sync AFTER INSERT OR UPDATE OF content, org_id ON knowledge_chunks
    FOR EACH ROW EXECUTE FUNCTION knowledge_chunk_search_sync();
