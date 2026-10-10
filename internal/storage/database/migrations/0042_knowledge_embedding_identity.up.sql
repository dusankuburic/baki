-- baki:compat expand
-- Embedding identity on knowledge chunks and the chunker version on knowledge documents (ADR 0027). A chunk's
-- embedding_identity is the provider's frozen identity string (label|baseURL|model|dims, or gemini|model) of the
-- embedder that produced its vector; NULL is a legacy row embedded before this column existed, matched leniently
-- by searches and stamped once an upload or re-index proves the org's current model reproduces its vectors. A
-- document's chunker_version names the chunking geometry it was split with (1: 1000-rune chunks, 15 % overlap).
-- No index: the org's chunks are already walked by knowledge_chunks_org_doc_idx. The release before ignores
-- both columns.
ALTER TABLE knowledge_chunks    ADD COLUMN IF NOT EXISTS embedding_identity TEXT;
ALTER TABLE knowledge_documents ADD COLUMN IF NOT EXISTS chunker_version    INT NOT NULL DEFAULT 1;
