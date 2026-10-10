-- Reverses v42. The identity and version columns go; the release before never reads them, and searches return
-- to matching by vector width alone.
ALTER TABLE knowledge_chunks    DROP COLUMN IF EXISTS embedding_identity;
ALTER TABLE knowledge_documents DROP COLUMN IF EXISTS chunker_version;
