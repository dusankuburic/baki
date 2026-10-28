-- Reverses v51. A pending upload cannot be finished by the release before, and its chunks still to embed have no
-- embedding, which the restored NOT NULL refuses: it goes with its chunks (the ready version of its filename it would
-- have replaced still serves). The source texts go too: the release before re-chunks nothing.
DELETE FROM knowledge_documents WHERE state = 'pending';
ALTER TABLE knowledge_chunks    ALTER COLUMN embedding SET NOT NULL;
ALTER TABLE knowledge_chunks    DROP COLUMN IF EXISTS pending;
ALTER TABLE knowledge_documents DROP COLUMN IF EXISTS content;
ALTER TABLE knowledge_documents DROP COLUMN IF EXISTS state;
