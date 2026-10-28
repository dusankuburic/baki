# 44. Knowledge retrieval that explains itself, finds exact terms, and uploads in pages

Date: 2026-10-27. Status: accepted, implemented (AI roadmap step 10). Amends ADR 0027 (§3 the stamp, §9 the lease) and builds on ADR 0038 (tool access) and ADR 0036 (data frames).

## Context

An organisation's knowledge base reached the model through one pre-turn lookup with an 800 ms deadline, keyed on the user's message alone. Several problems followed:

- **No reason when it found nothing.** A member whose key was for another provider, a slow provider and a member with no key at all all looked like "no guidelines". The lookup was counted `skipped` or `error`, and the user was not told.
- **Short questions without context.** A short question about the selected action ("is this allowed?") was embedded on its own and matched nothing.
- **Exact terms missed.** Retrieval was dense-only, so an exact PAD action name, a `%Variable%` or an error code could miss the one chunk that named it.
- **No tool for the model.** The tool loop could not consult the knowledge base once it knew which rule mattered.

On the ingest side:

- **One unpaged upload request.** Up to 2,500 chunks had to embed inside the 240 s Azure ingress cap. A client that was cut off never learned whether the document was written.
- **Chunking could not change.** The source text was discarded and the version-1 sentence splitter altered the text ("3.5 mm" became "3. 5 mm").
- **Unproven identity stamp.** The legacy identity stamp turned a three-document sample into an org-wide claim.

## Decision

1. **Every lookup says why.**
   - `pad_chat_rag_lookups_total{source, outcome}`:
     - Sources: `turn`, `tool` and `preview`.
     - Outcomes: `non_member`, `trivial`, `no_embedder`, `identity_mismatch`, `deadline`, `error`, `miss` and `hit`.
   - `pad_chat_rag_lookup_seconds{phase}` times the `resolve`, `embed` and `search` phases.
   - When the caller's embedding identity sees no chunk of a non-empty corpus, the lookup returns `rag.ErrEmbedderMismatch` before any embedding call.
   - A turn whose lookup left the guidelines out for a reason the user or an admin can act on emits one journaled `notice` `{kind: "knowledge", status}` before its first chunk. The statuses are `identity_mismatch`, `no_embedder` and `deadline`. The answer stores the status, so a reload shows it too.
   - Members still embed with their own key unless the operator pins `PAD_EMBEDDING_PROVIDER`. That choice is kept (ADR 0027); the notice and the knowledge panel explain it.
2. **The deadline is the operator's.** `PAD_RAG_DEADLINE_MS` sets it; the default stays 800 ms.
3. **Resolution is memoised and invalidated.**
   - **The resolved embedder:** cached per caller and organisation for 60 s. It is dropped on credential changes, including Azure AI Foundry connection writes, and on policy, membership and settings changes. An unreadable key store is never memoised; it counts as `error`, not `no_embedder`.
   - **The identity census and the stranded-by-document map:** cached per corpus generation.
   - **The empty-result census:** runs after the result is delivered.
4. **The query carries the selection.** It is the question plus a scrubbed `Selected: …` line naming:
   - the selected action;
   - its subflow;
   - its top finding rules.

   A message under 12 runes with nothing selected is `trivial` and is not embedded.
5. **Retrieval is hybrid.**
   - **A lexical side table** (`knowledge_chunk_search`, migration 0050) holds each chunk's text under a trigram index, kept in step by a trigger and backfilled after boot. The chunk table's forced row-level security keeps the planner from using a trigram index on it directly.
   - **Each phrase is probed separately** through that index and joined to the visible chunks before its cap.
   - **Reciprocal rank fusion** merges the dense and lexical lists, so an exact term reaches its chunk even below the vector floor.
6. **The model can search.** `search_knowledge{query, limit}` is a read-only tool:
   - It is offered only on an organisation's flow with the knowledge service wired, and mentioned in the system prompt only then.
   - Its results are scrubbed and capped like the pre-turn guidelines, and its query has its own 15 s timeout.
7. **Uploads are two-phase and resumable** (migration 0051).
   - The upload stores the document pending, with its source text and chunk text, then embeds and commits it page by page within the request budget.
   - `{docId, cursor?}` on the same route continues it. Pending chunks are never searched.
   - A re-upload of the same text resumes; a re-upload of different text supersedes the unfinished one, deleted before anything new is stored.
   - Unfinished uploads older than 30 days are swept by the retention purge.
8. **Chunking can evolve.** Chunker version 2 is heading-aware for Markdown and never alters the text. A `rechunk` re-index re-cuts documents that kept their source. It never touches a name with an unfinished upload, and skips documents stored before v51, which have no source.
9. **The legacy stamp is per document** (ADR 0027 §3 amendment). Only proven documents are stamped, with an `org.knowledge_identity_stamp` audit row naming them.
10. **Upload filenames are validated.** A name must be non-empty, at most 255 bytes, contain no control, format or separator characters, and no path separators (400 `KNOWLEDGE_FILENAME_INVALID`). The source label rendered in the prompt neutralises brackets.

## Consequences

- An operator can tell policy, latency and misconfiguration apart. A user is told when the organisation's guidelines were not consulted, and why.
- An upload never depends on one request outliving the ingress cap. A stopped or cut-off upload is listed as unfinished and resumes without paying twice.
- The lexical index costs a trigger on chunk writes and a side table about the size of the chunk text. The post-boot backfill fills it for chunks stored before v50.
- A change to the registry's "Use for knowledge base" designation is announced to no cache, so only the memo's 60 s TTL bounds it.
- An upload's age counts from when it was first stored, so one still being continued 30 days later can be swept mid-way.
- Version-1 documents without their source stay at version 1 until re-uploaded.
