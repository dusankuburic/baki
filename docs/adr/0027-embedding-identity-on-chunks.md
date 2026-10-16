# 27. Embedding identity on knowledge chunks, chunker version on documents

Date: 2026-10-10. Status: accepted, implemented (Phase 4b, RAG).

## Context

Nothing recorded which model produced a stored vector: text-embedding-3-small and Azure ada-002 are both 1536 wide, so a corpus re-pointed to another model at the same width read "searchable" while every search missed. The only mismatch detector was a billed sample call; a re-index re-walked already-converted chunks; the per-org mutation lock was process-local.

## Decision

1. Identity per chunk, version per document: `knowledge_chunks.embedding_identity TEXT NULL`, `knowledge_documents.chunker_version INT NOT NULL DEFAULT 1` (0042, expand).
2. The stored identity is the provider's string, as reported, format frozen: `label|baseURL|model|dims` (OpenAI-compatible) and `gemini|model` (`ai.EmbeddingIdentity`, pinned by `TestEmbeddingIdentity_FormatFrozen`). A spec-derived identity must keep producing the same string or ship a stamped rename. The string is never parsed for width — `vector_dims` stays the width check.
3. NULL is legacy, matched leniently (`embedding_identity = $current OR IS NULL`, never worse than before). The sample guard stays only for rows without an identity; when a sample proves the caller's model reproduces legacy rows, `StampKnowledgeChunkIdentity` rewrites the org's NULL rows, ending the lenient match per org.
4. Writes carry the identity: `AddDocument` stamps every chunk and the document's `rag.chunkerVersion` (1; a geometry change bumps it and adds a golden). A re-index page is stamped with the identity it was embedded under.
5. Re-index walks only what is not at the caller's model: unforced, `ListKnowledgeChunkContents(..., notIdentity)` skips chunks already under the caller's identity; forced, every chunk is rewritten (an Azure deployment re-pointed behind the same name keeps its identity).
6. Precise empty-result classification via the identity census (`CountKnowledgeChunksByIdentity`): chunks under another identity count `pad_rag_stranded_search_total` (new); nothing at the configured width counts `pad_rag_dim_mismatch_total`; below the floor counts nothing.
7. A declared on-demand re-embed job, never automatic spend: `knowledge_reembed` (`lifecycle.DeclareJob`, derived key, on demand) runs `ReindexOrg` page after page as the admin who started it (RLS) under their credential scope. `POST /api/orgs/{id}/knowledge/reindex` with `{"background": true}` answers 202; refused 400 `EMBEDDING_ORG_NOT_CONFIGURED` unless the embedder resolved to the org's shared credential (`ai.UsedOrgCredential`) — a member's personal key is not spent unattended. One job for every org: its lease serialises runs (409 `KNOWLEDGE_BUSY`; 503 `KNOWLEDGE_JOB_UNAVAILABLE`). A detected model switch is counted and shown, never self-repaired.
8. Admin surface: `GET /api/orgs/{id}/knowledge` carries `embeddingStranded` per document and `chunkerVersion`.
9. Synchronous writes take the org's advisory lock (R2): `SaveKnowledgeChunks` and `UpdateKnowledgeChunkEmbeddings` take `pg_advisory_xact_lock(hashtext(org_id))` in their own transaction, serialising against other replicas.

## Consequences

Uploads into a stamped corpus and re-indexes of one no longer bill the sample call. Storage contract grows the identity filters and the census/stamp methods; `Knowledge_chunks_carry_their_embedding_identity` holds Postgres and the fake to the same rules. Rollback: expand; both columns ignored by the previous release. One hazard: a re-index run on the previous release during a rollback rewrites vectors without touching identities — if that happened, run `bakicli migrate down 42` before rolling forward, or force a re-index after it. Open: an Azure deployment re-pointed behind the same name is undetectable (the admin forces a re-index).
