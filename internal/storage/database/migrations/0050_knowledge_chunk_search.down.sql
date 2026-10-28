-- Reverses v50: the trigger, its function and the side table go (its indexes with it). The release before never
-- reads them; knowledge_chunks is untouched.
DROP TRIGGER IF EXISTS knowledge_chunk_search_sync ON knowledge_chunks;
DROP FUNCTION IF EXISTS knowledge_chunk_search_sync();
DROP TABLE IF EXISTS knowledge_chunk_search;
