-- baki:compat expand
-- Two-phase uploads and re-chunkable documents (R-knowledge-2, R-knowledge-9). An upload first stores its document
-- (state 'pending') and every chunk's text, the chunks pending and without an embedding (NULL), then embeds them page
-- by page, each page committing on its own, so a request the ingress cuts off keeps what it bought and a
-- continuation embeds the rest. A pending chunk is not in the corpus: no search returns it, embedded or not, and the
-- census, the guard's samples and the re-index pages leave it out. When no chunk of the document lacks its embedding
-- the document turns ready, its chunks join the corpus and the older versions of its filename go, in one
-- transaction. content keeps the document's (masked) source text, so a chunker change can re-chunk it; NULL for
-- every document stored before this step, which keeps its chunks as they are.
--
-- Expand: three columns with constant defaults (or none) and a dropped NOT NULL, catalogue changes. The release
-- before writes none of them (its documents are ready, its chunks live and embedded, no source kept); its searches,
-- samples and width probes already skip a NULL embedding (no width, not an array), so it never returns a chunk
-- still to embed. Rolled back to it while an upload is pending, it searches that upload's embedded chunks beside
-- the version they replace, and its re-index embeds the rest without finishing the document (the continuation
-- finishes it); the down step removes pending uploads.
ALTER TABLE knowledge_documents ADD COLUMN IF NOT EXISTS state   TEXT NOT NULL DEFAULT 'ready';
ALTER TABLE knowledge_documents ADD COLUMN IF NOT EXISTS content TEXT;
ALTER TABLE knowledge_chunks    ADD COLUMN IF NOT EXISTS pending BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE knowledge_chunks    ALTER COLUMN embedding DROP NOT NULL;
