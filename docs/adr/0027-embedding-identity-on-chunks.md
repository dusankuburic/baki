# 27. Embedding identity on knowledge chunks, chunker version on documents

Date: 2026-10-10. Status: accepted, implemented (Phase 4b, RAG).

## Context

The org knowledge base is one corpus embedded by whichever provider the deployment pins, the org configured or
the caller has a key for (`rag.KnowledgeService.embedderFor`), and nothing recorded which model produced a stored
vector. Equal width does not mean equal model: text-embedding-3-small and an Azure ada-002 deployment are both
1536 wide, so a corpus re-pointed to another model at the same width read "searchable" (`vectorIndexed` full)
while every search missed (R1). The only mismatch detector was a billed sample: up to three chunks re-embedded and
compared on every upload into a non-empty corpus and on the first request of every re-index (R5); the width
metric `pad_rag_dim_mismatch_total` counted every empty result of a non-empty org, legitimate misses included (R4);
a re-index walked every chunk, also those a previous run had already converted (R6); and the per-org mutation
lock was process-local (R2).

The chunker's geometry (1000-rune chunks, 15 % overlap) was not versioned either. The original text is not kept:
chunks are the only copy, so a geometry change cannot be applied to stored documents, while `stripOverlapPrefix`
assumes the geometry at retrieval time.

## Decision

1. **Identity per chunk, version per document.** `knowledge_chunks.embedding_identity TEXT NULL` and
   `knowledge_documents.chunker_version INT NOT NULL DEFAULT 1` (migration 0042, expand). A re-index page can split
   a document across requests and a resumed run may finish under another model, so only the chunk is exact for
   the identity; chunking happens once per document, so the version is the document's.
2. **The stored identity is the provider's string, as reported, and its format is frozen:**
   `label|baseURL|model|dims` for the OpenAI-compatible family and `gemini|model` for Gemini
   (`ai.EmbeddingIdentity`; `TestEmbeddingIdentity_FormatFrozen` pins it). A spec-derived identity must keep
   producing the same string or ship a stamped rename, or every stored corpus strands. The string is never parsed
   for the width: a provider may retry at its native width without changing its identity (R3), so the existing
   `vector_dims` / `jsonb_array_length` filters stay the width check.
3. **NULL is legacy, matched leniently, stamped on the first proof.** The migrator cannot know the identity (the
   embedder is resolved per org and per caller at runtime). Searches match
   `embedding_identity = $current OR embedding_identity IS NULL`, never worse than before. The sample guard stays
   only for rows without an identity, and for providers that name none; a corpus whose every row carries the
   caller's identity is accepted with no provider call, rows under another identity are another model whatever
   the sample says (the sample only tells another width — the stranded-corpus recovery — from another model).
   When the sample proves the caller's model reproduces the legacy rows, `StampKnowledgeChunkIdentity` rewrites
   the org's NULL rows to that identity: the lenient match ends per org after one proven upload or re-index.
4. **Writes carry the identity.** `AddDocument` stamps every chunk with the embedder's identity and the document
   with `rag.chunkerVersion` (1; `TestChunkText_GoldenGeometry` pins the split under it — a geometry change bumps
   the version and adds a golden). A re-index page is stamped with the identity it was embedded under.
5. **Re-index walks only what is not at the caller's model, unless forced.** Unforced,
   `ListKnowledgeChunkContents(..., notIdentity)` skips the chunks already under the caller's identity (legacy
   rows are walked), so a restarted or repeated run never re-buys a page and a corpus the proof just stamped has
   nothing left. Forced, every chunk is rewritten: the admin's word that the vectors must change even where the
   identity would not (an Azure deployment re-pointed behind the same name keeps its identity, R8).
6. **Precise empty-result classification.** An empty search asks the identity census
   (`CountKnowledgeChunksByIdentity`): chunks under another identity count `pad_rag_stranded_search_total` (new);
   nothing in the vector index at the configured width counts `pad_rag_dim_mismatch_total` as before; a corpus with
   nothing above the relevance floor counts nothing. Both are logged with the org.
7. **A declared, on-demand re-embed job; never automatic spend.** `knowledge_reembed` (`lifecycle.DeclareJob`,
   derived key, "on demand") runs `ReindexOrg` page after page until no cursor comes back, as the admin who started
   it (RLS) and under their credential scope. `POST /api/orgs/{id}/knowledge/reindex` with `{"background": true}`
   starts it and answers 202 `{status: accepted, job: knowledge_reembed}`; progress is the job's run state
   (`job_runs`, `pad_job_*`) and the document list's `embeddingStranded` counts. It runs unattended, so it is
   refused with 400 `EMBEDDING_ORG_NOT_CONFIGURED` unless the embedder resolved to the organisation's shared
   credential (`ai.UsedOrgCredential`): a member's personal key is not spent after they left, and the org owns its
   corpus; the synchronous path stays open to them. One job for every org: its lease serialises runs across
   replicas, one org at a time (409 `KNOWLEDGE_BUSY` meanwhile; 503 `KNOWLEDGE_JOB_UNAVAILABLE` when the lock could
   not be asked for or the server is stopping). A detected model switch is counted and shown, never repaired by
   itself.
8. **The admin surface.** `GET /api/orgs/{id}/knowledge` carries `embeddingStranded` per document (chunks under
   another identity than the viewer's; absent when none, or when the viewer has no embedder to compare with) and
   `chunkerVersion`; the Knowledge Base panel shows "indexed with another embedding model; re-index" next to the
   width warning.
9. **The synchronous writes take the org's advisory lock (R2).** `SaveKnowledgeChunks` and
   `UpdateKnowledgeChunkEmbeddings` take `pg_advisory_xact_lock(hashtext(org_id))` in their own transaction, so
   an upload's insert and a re-index page of one org serialise across replicas, where the process-local
   `lockOrg` cannot reach.

## Consequences

- Uploads into a stamped corpus and re-indexes of one no longer bill the sample call; only legacy corpora do,
  once. A same-width model switch is visible at once (stranded counts, the new metric) instead of at the next
  write.
- The storage contract grows by the identity on `SearchKnowledge` / `ListKnowledgeChunkContents` and by
  `CountKnowledgeChunksByIdentity`, `CountStrandedKnowledgeChunksByDocument` and `StampKnowledgeChunkIdentity`;
  `CountKnowledgeChunks` keeps its signature (the Azure registry service consumes it through a narrow interface).
  The scenario `Knowledge_chunks_carry_their_embedding_identity` holds Postgres and the fake to the same rules.
- A corpus that mixes stamped and legacy rows (a stamp that failed, an upload from another replica mid-rollout)
  is proven by a sample that may pick only stamped rows; it heals on the next re-index. Rare and self-correcting.
- Postgres stays the only backend with the columns; the fake mirrors them in memory.

## Rollback

Expand step: the previous release ignores both columns (its inserts leave them at NULL and 1), its searches match
by width alone as before, and a client of either release works against the other — `embeddingStranded`,
`chunkerVersion` and `background` are optional additions, the 409 codes and the resume cursor are unchanged. The
down step drops both columns; a corpus stamped by this release loses nothing but the identities.

One hazard while rolled back: the previous release's re-index rewrites `embedding` / `embedding_vec` without
touching `embedding_identity`, so a re-index to another model on the old binary leaves this release's identities
stamped over the other model's vectors. Rolled forward, the column-first guard trusts the column: the rows count as
current, no sample runs, and an unforced re-index finds nothing to convert while every search under the stamped
identity ranks the wrong vectors. If a knowledge re-index ran on the previous release, run `bakicli migrate down 42`
before rolling forward (the identities are dropped; the legacy path re-proves them), or force a re-index of the
affected organisations after it.

## Open

- The re-index resume guard (`reindexGuard` with a cursor) still re-embeds a sample of the converted prefix; the
  prefix's identity could answer it without a provider call once the samples carry the column.
- The di-only test graphs have no job leaser, so the background job runs there without the deployment's lease
  (`di.ProvideKnowledgeService` wires `rag.WithJobLeaser` whenever one is provided); the per-org advisory lock
  keeps two replicas' pages from interleaving either way.
- A deployment without pgvector reports every empty result of a non-empty org on the width metric, as before
  (`VectorIndexed` is always 0 there).
- An Azure deployment re-pointed behind the same name is undetectable by identity (R8); the admin forces a
  re-index, which now rewrites every chunk.
