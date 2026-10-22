# AI roadmap: Organisation knowledge

Items of category `knowledge` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-knowledge-1

**Unpinned deployments never embed with the org's shared key: members without a personal key get no guidelines, members on another provider search an invisible corpus** · tier 1

**Problem.** `embedderFor` prefers the organisation's shared credential (`ForEmbeddingOrg`, `orgFirst`) only in two cases: the provider is registry-embedded (Azure), or it is pinned by `PAD_EMBEDDING_PROVIDER`. For the default settings provider it calls `ForEmbedding` (`memberOnly`), then scans `EmbeddingFallbackOrder` on the member's own keys. This is a recorded and tested design choice ("an org shares one deliberate credential, not a fallback").

Take a typical cloud deployment with an org-level OpenAI key and no pin:
- A member without a personal OpenAI key gets `ErrEmbeddingUnavailable` on every chat turn. It is recorded as `outcome=error` and never logged.
- A member whose only key is Gemini embeds the query under `gemini|...`, so the identity filter returns nothing. The only trace is a WARN plus `pad_rag_stranded_search_total`.

The admin who uploaded sees a healthy KB, and each member silently gets no guidelines. An operator workaround exists today: `PAD_EMBEDDING_PROVIDER=openai` makes the org key first. But the docs never tell operators they need it. The gap also reaches ADR 0027 §7: the background re-index requires `ai.UsedOrgCredential`, which can never be true for an unpinned non-Azure provider.

**Evidence.**
- `internal/rag/service.go:256` — `ForEmbeddingOrg` only when `registryEmbedded(pid)` or `pid` is the operator pin; otherwise `ForEmbedding` (member-only) at :259.
- `internal/rag/service.go:312` — the fallback scan over `EmbeddingFallbackOrder` runs on the member's keys; :320 returns `ErrEmbeddingUnavailable` when none has a key.
- `internal/ai/factory.go:427` — `ForEmbedding` builds with `memberOnly`; `ForEmbeddingOrg` (384-385) is `orgFirst`.
- `internal/storage/database/postgres_knowledge.go:394` — search filters candidates to the caller's identity or NULL.
- `internal/service/chat_context.go:237` — a Search error records `recordRAGLookup("error")` (238-240) with no log line, and the guidelines are dropped.
- `internal/rag/service.go:725` — `explainEmptySearch`. Its stranded branch (731-734) is only a WARN plus a counter when the caller's identity sees nothing.
- `internal/rag/org_embedder_test.go:87` — `TestEmbedder_PinnedProviderOrgFirst_DefaultStaysPersonal` (87-112) asserts that the org's `openai` key is not used when nothing is pinned, and that a member keyed only on `glm` falls back to `glm`.
- `internal/config/config.go:331` — `PAD_EMBEDDING_PROVIDER` pins the deployment's embedding provider over the settings value (331-334).
- `docs/deployment.md:162` — lists `PAD_EMBEDDING_PROVIDER` as the deployment-wide embedding provider, without saying that an unpinned deployment never embeds on an org's shared key.
- `docs/adr/0027-embedding-identity-on-chunks.md:17` — §7: the background re-index is refused unless the embedder resolved to the org's shared credential (`ai.UsedOrgCredential`).

**Change.** Keep the deliberate member-only resolution for unpinned providers (`org_embedder_test.go:87-111`). An org-level corpus embedder (persisting the org's embedding provider and resolving org-first from it) would reverse that recorded choice. It needs a new ADR and an update to that test, and it is not part of this item. Scope the item to three changes:
1. Log the Search error at WARN, rate-limited per org, and count `ErrEmbeddingUnavailable` as its own outcome, separate from `error`.
2. Return a typed identity-mismatch outcome (`rag.ErrEmbedderMismatch`) before embedding when the member's identity has no chunks, and count it as its own outcome.
3. Surface a knowledge status in the stream metadata and in `KnowledgeBasePanel`. Include a hint that the operator can set `PAD_EMBEDDING_PROVIDER` to embed on the org's shared key.

**Touches.** `internal/rag/service.go`, `internal/service/chat_context.go`, `internal/service/metrics.go`, `frontend/src/components/settings/KnowledgeBasePanel.tsx`, `frontend/src/components/chat/`

**Done when.**
- Only if a new ADR adopts org-first resolution for unpinned providers: with an org-level `openai` key, no `PAD_EMBEDDING_PROVIDER` pin and a member with no personal key, Search embeds the query under the org's key and returns hits. The test counts `orgCalls == 1` and `memberCalls == 0`, and `TestEmbedder_PinnedProviderOrgFirst_DefaultStaysPersonal` is updated.
- A member keyed only on another provider gets `pad_chat_rag_lookups_total{outcome="identity_mismatch"}` and zero provider `Embed` calls for that turn.
- The stream metadata of such a turn carries a knowledge status that the frontend renders, covered by a snapshot test on the chat notice.

### R-knowledge-2

**Upload is one unpaged request: up to 2 500 chunks must embed inside the ingress cap, and a client cut off never learns whether the document was written** · tier 1

**Problem.** `AddDocument` chunks the document and embeds every batch: up to 25 batches of 100, three in flight, each subject to retries and the split cascade. Only then does it write the document and its chunks in one transaction, so nothing is persisted until every batch succeeds. The upload also holds the org lease for the whole embed window, which blocks re-index for that time.

The route runs `NoTx` under `route.Bulk()`. Its comment says both bulk routes commit page by page, which is true for re-index only. The frontend waits 5.5 min, but `docs/deployment.md` states that the Azure Container Apps ingress cuts every request at 240 s, with no knob, and that uploads do not chunk around it.

A large upload under throttling therefore buys embeddings for nothing once the ingress drops the response. One of two things follows:
- The server finishes and writes. The admin can learn this only by re-listing documents, and may re-upload, paying again and superseding their own document.
- The server is cancelled and writes nothing.

The documented mitigation is to split large documents by hand.

**Evidence.**
- `internal/rag/service.go:84` — `maxKnowledgeChunks = 2500`; :88 `embedBatchSize = 100`; :957 `embedConcurrency = 3`.
- `internal/rag/service.go:554` — `embedBatched` runs for every chunk before `SaveKnowledgeDocumentWithChunks` at :593.
- `internal/rag/service.go:518` — the org lease is held over the check → embed → write window (518-525).
- `internal/api/org_knowledge.go:363` — upload runs `NoTx` under `route.Bulk()`. The comment at 360-362 says both bulk routes commit page by page.
- `frontend/src/components/settings/KnowledgeBasePanel.tsx:39` — `KNOWLEDGE_BULK_TIMEOUT_MS` = 5.5 min.
- `docs/deployment.md:140` — the ACA ingress caps requests at 240 s. Uploads do not chunk around it, so the doc says "split large documents".

**Change.** Make upload two-phase and resumable:
- `POST /upload` stores the document and its chunk text with `embedding_identity` NULL and a new `knowledge_documents.state='pending'` (expand migration), and answers 201 with the doc id.
- It then embeds the document's chunks page by page with the `ReindexOrg` cursor machinery scoped to one `doc_id`. It does this synchronously up to the request budget, returning `nextCursor` on truncation as re-index does, or through the background job.
- Search excludes pending rows (a NULL `embedding_vec` already does).
- The list reports `pendingChunks`.
- A re-upload of the same filename supersedes a pending document.

Correct the route comment at `internal/api/org_knowledge.go:360-362` with the fix.

**Touches.** `internal/rag/service.go`, `internal/api/org_knowledge.go`, `internal/storage/interfaces/knowledge.go`, `internal/storage/database/postgres_knowledge.go`, `internal/storage/database/migrations/`, `internal/testutil/fakebackend.go`, `frontend/src/components/settings/KnowledgeBasePanel.tsx`, `docs/deployment.md`

**Done when.**
- An upload of 2 500 chunks, with an embedder that sleeps 1 s per batch, returns within the request budget with a `nextCursor`. Following the cursor completes the document, and the list shows `pendingChunks` decreasing.
- Cancelling the client mid-upload leaves a pending document whose committed pages are kept and resumable. No embedding is paid twice (count `Embed` inputs).
- A pending document's chunks are never returned by `SearchKnowledge`.

### R-knowledge-7

**Knowledge documents are embedded and stored unscrubbed; the secret scrub runs only when chunks are rendered into a turn** · tier 1

**Problem.** `AddDocument` chunks the uploaded text, sends every chunk verbatim to the embedding provider, then stores the raw chunk text. `ReindexOrg` re-sends the stored raw text. The only `ScrubText` in the rag package is on the search query. A runbook or guideline that contains a connection string, API key or password is therefore exposed in two ways:
- It leaves the deployment in clear text to the embedding vendor, on upload and on every re-index.
- It sits unmasked in `knowledge_chunks` for anyone with database access.

Retrieved guidelines are scrubbed before any member or model sees them, but only at render time. The scrubber already exists and is cheap relative to the embed call.

**Evidence.**
- `internal/rag/service.go:513` — `chunks := chunkText(content, ...)`, then `embedBatched(ctx, provider, chunks)` at :554 with no `ScrubText`. The raw chunk is stored as `Content` (582).
- `internal/rag/service.go:851` — the re-index page loop re-embeds the stored raw text.
- `internal/rag/service.go:687` — the only scrub in the package: `query = scrubber.ScrubText(query)`.
- `internal/service/chat.go:617` — the stream path scrubs guidelines before they enter the request.
- `internal/service/chat_context.go:223` — `buildScrubbedContext` scrubs guidelines on the synchronous path, and the context preview uses it (632-636).
- `internal/rag/service.go:1070` — `stripOverlapPrefix` (1070-1095) de-duplicates neighbouring chunks by exact match on the 150-rune overlap (`chunkOverlapRunes`, 1051-1055).

**Change.**
- In `AddDocument`, scrub the document text and the filename with `scrubber.ScrubText` before chunking, so neighbouring chunks share an identical overlap and `stripOverlapPrefix`'s exact-match de-duplication keeps working. If chunks were scrubbed independently, a secret that straddles the 150-rune overlap could be masked differently in neighbouring chunks.
- In the re-index page loop, re-embed from scrubbed text and store it, and account for the same overlap effect on legacy raw chunks.
- Count masked chunks in a metric.
- Keep the render-time scrub as defence in depth.
- Document in ADR 0027 and in the KB panel copy that uploaded documents are masked for secret patterns.

**Touches.** `internal/rag/service.go`, `internal/rag/reindex*.go`, `internal/rag/reembed_job.go`, `internal/rag/service_test.go`, `docs/adr/0027-embedding-identity-on-chunks.md`, `frontend/src/i18n/en.settings.ts`

**Done when.**
- A rag test uploads a document containing `Password=hunter2x...` and asserts that the embed request body and the stored chunk carry the mask, not the value.
- A re-index test shows that a legacy raw chunk is re-embedded from its scrubbed text.
- `pad_rag_scrubbed_chunks_total` (or similar) is in the metrics golden.

### R-knowledge-6

**The knowledge base is consulted once pre-turn (800 ms, 1 200 tokens, keyed on the user message) and the tool loop cannot ask it** · tier 2

**Problem.** RAG guidelines are fetched once per turn, in a goroutine started by `prepareTurn`. The fetch runs under an 800 ms deadline and a 1 200-token cap, uses the user's message alone, and is gated on org membership. The result is prepended to the single user turn. When the deadline skips the fetch, the turn proceeds silently: outcome=skipped, with no notice. In the tool loop the model learns from `list_findings`/`get_block` which rule, connector or pattern actually matters. At that point the registry offers no knowledge tool. Org-specific policies ("always wrap SQL in retry with 3 attempts") therefore never reach the fix discussion unless the opening message happened to match. For an org that curates PAD standards in the knowledge base, the assistant answers from the first question's wording only.

**Evidence.**
- `internal/service/chat_context.go:33` — `ragGuidelinesDeadline` is 800 ms; `ragGuidelinesTokenCap` is 1200 (36)
- `internal/service/chat_context.go:227` — `ragGuidelines` is gated on membership and keyed by `userMessage`; a deadline skip is recorded as "skipped" with no user-visible notice (239-242)
- `internal/service/chat.go:555` — the RAG goroutine is started once in `prepareTurn` with the question text (555-572)
- `internal/service/chat_context.go:395` — guidelines ride in the single user turn
- `internal/ai/tools.go:263` — `buildToolRegistry` has no knowledge or search tool
- `internal/ai/testdata/tools.golden:2` — eight tools are declared (2-9), and none touches the knowledge base
- `internal/service/chat.go:885` — `buildStreamRequest` runs once before either tool loop starts (898-923)

**Change.** Add a read-only `search_knowledge{query, limit}` ToolSpec backed by the same `knowledge.Search`. It keeps the membership gate, the scrub, the token cap and the budget hook (the query embed is billed). It returns at most one page of chunks with source titles, and it is counted by `recordRAGLookup`. Offer it only when the flow has an organisation and the knowledge service is wired, and mention it in the system prompt when an org knowledge base exists. Report a pre-turn deadline skip as a Notice row, so the user knows the guidelines were not consulted.

**Touches.** `internal/ai/tools.go`, `internal/ai/testdata/tools.golden`, `internal/service/chat.go`, `internal/service/chat_context.go`, `internal/ai/context.go`, `internal/rag/knowledge.go` (new), `internal/service/chat_rag_gate_test.go`, `internal/service/chat_tools.go` (new), `internal/rag/service.go`, `internal/service/metrics.go`

**Done when.**
- A scripted model calls `search_knowledge` mid-loop and receives scrubbed chunks within the token cap. A caller outside the flow's org receives "not available", and `recordRAGLookup("skipped")` increments.
- The tool's embed call goes through the budget hook. With an exhausted budget the tool result says so and the stream continues. Desktop and local mode with no knowledge service wired do not offer the tool.
- A RAG deadline skip emits a notice row that is visible in the trail.

### R-knowledge-8

**Dense-only retrieval: exact PAD action names, variable names and error codes in a question do not reach the chunk that names them** · tier 2

**Problem.** `SearchKnowledge` ranks by cosine distance only, through HNSW, an exact scan or the Go fallback, with a 0.5 floor. Organisation guidelines for PAD are full of identifiers that dense embeddings represent poorly, especially when the question is short. Examples are action display names like "Run DOS command", `%Variable%` names, connector names, error codes and file names like `flow.pad`. `pg_trgm` has been installed since v13 and flow search uses it. `knowledge_chunks` has no lexical index and no lexical path, so a question quoting the exact term can miss the one chunk that contains it.

**Evidence.**
- `internal/storage/database/postgres_knowledge.go:291` — `SearchKnowledge`: the vector path or the Go cosine fallback, nothing lexical
- `internal/storage/database/postgres_knowledge.go:362` — the exact statement orders by `embedding_vec <=>` only; the indexed statement at 385-401 does the same
- `internal/storage/database/postgres_migrations.go:1543` — `CREATE EXTENSION pg_trgm` since v13; trigram indexes on `flow_search` at 2088-2089, none on `knowledge_chunks` (grep over `internal/storage/database`)

**Change.** Move to hybrid retrieval. Add an expand migration with a GIN index on `knowledge_chunks.content` (pg_trgm or a tsvector expression). Add a lexical top-k statement filtered by org and identity. In `Search`, before `assembleGuidelines`, fuse the two result lists with reciprocal rank fusion. Keep the vector floor for dense-only hits, and use a trigram similarity floor for lexical hits. Add a parity test on the fake backend.

**Touches.** `internal/storage/database/migrations/`, `internal/storage/database/postgres_knowledge.go`, `internal/storage/interfaces/knowledge.go`, `internal/rag/service.go`, `internal/testutil/fakebackend.go`, `internal/storage/contract/knowledge.go`

**Done when.**
- A query containing an exact action name present in one chunk returns that chunk, even when the fake embedder scores it below 0.5.
- Lexical hits respect the identity filter and RLS like vector hits (a contract scenario).
- A plan test shows the lexical statement uses the new index.

### R-knowledge-9

**Chunking cannot evolve: original text is discarded, the sentence splitter is naive, and chunker_version is stored but never acted on** · tier 2

**Problem.** A document is chunked once at upload and only the chunks are kept. Any change to the chunking therefore cannot be applied to stored documents: the 1000-rune/15 % geometry, the paragraph split on `"\n\n"`, or the sentence split. The comment says a change must bump `chunkerVersion`, but nothing reads the version except the list query. The sentence split only applies to a paragraph longer than `knowledgeChunkRunes` (`internal/rag/service.go:1160`). Inside such a paragraph it cuts at every '.', '!' and '?'. The pieces are then re-joined with a space (`service.go:1167-1171`), so "3.5 mm" is stored as "3. 5 mm" and "flow.pad" as "flow. pad". The stored text itself is altered, not just split at a bad boundary. The API already returns `chunkerVersion` (`internal/storage/interfaces/knowledge.go:66`, ADR 0027 §8), but the frontend `KnowledgeDoc` type omits it. Markdown headings are the structure most SOPs carry, yet they are not used to scope chunks, so a retrieved passage arrives without the section it belongs to. Improving retrieval quality for existing corpora would require every admin to re-upload every document.

**Evidence.**
- `internal/rag/service.go:1046` — "A document is chunked once — the original text is not kept"; `chunkerVersion = 1` at 1050
- `internal/rag/service.go:1111` — paragraphs are split on `"\n\n"` only
- `internal/rag/service.go:1193` — `splitSentences` cuts at '.', '!' or '?' unconditionally (1198)
- `internal/storage/database/migrations/0042_knowledge_embedding_identity.up.sql:10` — the `chunker_version` column exists (default 1)
- `internal/storage/database/postgres_knowledge.go:75` — `chunker_version` is read for the list only (grep: no other consumer in `internal/rag`)
- `frontend/src/components/settings/KnowledgeBasePanel.tsx:15` — the `KnowledgeDoc` type omits `chunkerVersion`

**Change.** Keep the source text in a new `knowledge_documents.content TEXT` column (expand migration; the 1 MiB cap is already enforced), so a re-chunk is possible. Implement chunker v2 behind `chunkerVersion=2`, with its own golden:
- It is heading-aware for Markdown and prepends the heading path to each chunk.
- Its sentence boundaries are safe for abbreviations, decimals and URLs, and it never re-joins pieces in a way that changes the text.
- It keeps the same size invariant.

Make `stripOverlapPrefix` read the chunk's document version. Extend re-index with a "rechunk" mode that re-chunks and re-embeds documents whose version is below the current one, stamping `chunker_version=2`. Add `chunkerVersion` to the frontend type.

**Touches.** `internal/rag/service.go`, `internal/rag/identity_test.go`, `internal/storage/database/migrations/`, `internal/storage/database/postgres_knowledge.go`, `internal/storage/interfaces/knowledge.go`, `internal/api/org_knowledge.go`, `frontend/src/components/settings/KnowledgeBasePanel.tsx`

**Done when.**
- Golden geometry tests for v1 and v2 both pass, and a v2 chunk of a Markdown file starts with its heading path.
- v2 does not split "3.5 mm" or "flow.pad".
- The rechunk re-index re-chunks a v1 document, which then lists `chunkerVersion=2`; a plain re-index leaves v1 documents intact.

**ADR.** ADR 0027 §4 (`docs/adr/0027-embedding-identity-on-chunks.md:14`) specifies the version bump and golden; this item adds the source text and the rechunk mode.

### R-knowledge-10

**The retrieval query carries no flow context and no minimum length: 'is this ok?' embeds nothing useful and every short message pays a billed embed** · tier 2

**Problem.** `ragGuidelines` passes `req.UserMessage` alone to `Search`. The selected block (`ContextBlockID`), its action type, the subflow name and the current findings are all resolved for the context text, but none of them reaches the embedding. Questions about a selected action are typically short and deictic ("is this allowed?", "fix this"). The query vector then matches nothing above the 0.5 floor, yet the turn still pays for an embedding call and an HNSW query. `Search` scrubs and truncates the query but has no minimum length, so "ok" or "thanks" is embedded too. Query vectors are cached for 10 minutes per caller, org, embedder identity and query SHA (`internal/rag/search_cache.go:42-44`, `169-186`). An org with an empty corpus skips the embed entirely (`internal/rag/service.go:664`). The cost therefore falls on every distinct short message in an org with a non-empty corpus.

**Evidence.**
- `internal/service/chat_context.go:222` — `buildScrubbedContext` hands `req.UserMessage` to `ragGuidelines`; `Search(..., userMessage)` at 235
- `internal/service/chat.go:571` — the streaming call site: `s.ragGuidelines(ragCtx, scope, doc, req.UserMessage)`
- `internal/rag/service.go:661` — `Search` scrubs and truncates the query (687-693) but has no minimum length and adds no context
- `internal/service/chat_context.go:357` — the selected block and subflow are resolved for the context text, not for retrieval

**Change.** Build the retrieval query in `ragGuidelines` from the user message plus a compact, scrubbed description of the selection: action display name and type, subflow name, and the block's top finding rule ids. Skip retrieval when the resulting query is under a small rune threshold and nothing is selected. Keep the query-embedding cache keyed on the SHA of the augmented query.

**Touches.** `internal/service/chat_context.go`, `internal/service/chat.go`, `internal/rag/service.go`, `internal/service/chat_rag_gate_test.go`

**Done when.**
- With `ContextBlockID` set, a turn hands `Embed` a query that contains the block's action name (checked with a recording embedder).
- A 3-rune message with no selection makes zero `Embed` calls and records outcome=miss (or a new "trivial" outcome).

### R-knowledge-13

**Only .txt/.md plain text can be indexed; SOPs in DOCX/PDF/HTML have no ingestion path** · tier 2

**Problem.** The panel's file input accepts `.txt` and `.md`, and the hint says so. The upload API takes a JSON body with a `content` string, so the server never sees bytes it could extract text from. RPA teams keep organisation guidelines, coding standards and SOPs overwhelmingly as Word, PDF or wiki exports. Today an admin must convert them by hand.

**Evidence.**
- `frontend/src/components/settings/KnowledgeBasePanel.tsx:312` — `accept=".txt,.md"`
- `frontend/src/i18n/en.settings.ts:425` — "Currently supports .txt and .md files up to 1MB."
- `internal/api/org_knowledge.go:81` — the upload body is `{filename, content string}`, decoded as JSON (81-87)

**Change.** On the server, accept multipart uploads of `.docx` (unzip and read the `document.xml` text), `.pdf` (pure-Go text extraction) and `.html` (strip tags, keep headings), each with its own size cap. Normalise the result to Markdown-like text, keeping headings for chunker v2, then reuse `AddDocument`. Answer 415 for unsupported types. Keep the JSON path for the existing client. In the same change, fix the swagger annotation on the handler: it says `@Accept multipart/form-data` (`internal/api/org_knowledge.go:65`), but the handler only decodes JSON (81-87).

**Touches.** `internal/api/org_knowledge.go`, `internal/rag/extract/` (new), `frontend/src/components/settings/KnowledgeBasePanel.tsx`, `frontend/src/i18n/en.settings.ts`, `docs/swagger.yaml`

**Done when.**
- Uploading a fixture `.docx` yields a document whose chunks contain its paragraph text; a `.pdf` fixture does the same.
- An `.exe` upload is refused with 415 before any embedding call.
- Extraction runs under the existing 1 MiB text cap after conversion (413 otherwise).

### R-knowledge-3

**The legacy identity stamp generalises a 3-document sample to every NULL row of the org, irreversibly** · tier 3

**Problem.** `checkCorpusModel` samples `corpusSampleSize` = 3 chunks. The sample uses `DISTINCT ON doc_id`, ordered unindexed-first then by `doc_id`, `LIMIT 3`, so it takes the first documents. It re-embeds them with the caller's model and, when all three match, calls `StampKnowledgeChunkIdentity`, which rewrites every `embedding_identity IS NULL` row of the org to the caller's identity. A pre-0042 corpus can be mixed at the same width beyond the sampled documents, for example after a partial re-index on the old release or with two admins on different 1536-wide models. Such a corpus is then labelled as one model. The other model's vectors become "current" and pass the identity filter as candidates, where a noise cosine cannot be told apart from a miss. They are never again counted as stranded.

This release can also produce the failure deterministically. The re-index walk converts in `(doc_id, id)` order (`postgres_knowledge.go:478-480`), and the sample takes the lowest `doc_id`s (`:516-519`). Take a legacy model-B corpus whose forced re-index to model A stopped after converting 3 or more documents. The background job ends on any failure or at `reembedRunTimeout` (`reembed_job.go:28`, `:133-135`), and the next start begins from a zero cursor (`:126`). If that next start is unforced, it runs `checkCorpusModel` (`service.go:926`), samples only the converted head and stamps the unconverted B tail as A. It then converts nothing, because skip is A (`service.go:806`).

No API un-stamps or re-stamps, so the only recovery is a forced full re-index.

**Evidence.**
- `internal/rag/service.go:61` — `corpusSampleSize = 3`
- `internal/rag/service.go:389` — `SampleKnowledgeChunkEmbeddings(ctx, orgID, corpusSampleSize)`
- `internal/rag/service.go:407` — `verdict == corpusSameModel` leads to `StampKnowledgeChunkIdentity(..., "", identity)` for the whole org at `:412`
- `internal/storage/database/postgres_knowledge.go:510` — sample statement: `DISTINCT ON (c.doc_id)` … `ORDER BY unindexed, doc_id LIMIT $2` (516-519), which takes the first n documents
- `internal/storage/database/postgres_knowledge.go:741` — `UPDATE knowledge_chunks … WHERE org_id = $1 AND embedding_identity IS NOT DISTINCT FROM NULLIF($2::text, '')` (741-743); with `from = ""` this is every legacy row
- `internal/storage/database/postgres_knowledge.go:478` — the re-index walk is keyset-ordered on `(doc_id, id)` and skips rows already at the target identity (478-480)
- `internal/rag/reembed_job.go:126` — each background run starts from a zero cursor; a failure ends the run (133-135); `reembedRunTimeout` is 2 h (28)
- `internal/rag/service.go:926` — an unforced re-index runs `checkCorpusModel`; skip is the caller's identity unless forced (806-809)

**Change.** Stamp only what was proven. Either sample one chunk from every document before stamping org-wide (a bounded loop over `ListKnowledgeDocuments` pages, one Embed call per 100 documents), or stamp per document and leave unsampled documents NULL, so the next upload or search keeps the lenient match for them. Record the stamp in the audit trail with the sampled document ids. No un-stamp API is needed, because a forced re-index already recovers: it skips nothing (`service.go:807`) and bypasses the model guard (`:923`). The identity format and ADR 0027's NULL-is-legacy rule stay unchanged. The sample-proven org-wide stamp in §3 needs an amendment. Tier 3.

**Touches.** `internal/rag/service.go`, `internal/storage/interfaces/knowledge.go`, `internal/storage/database/postgres_knowledge.go`, `internal/testutil/fakebackend.go`, `internal/rag/identity_test.go`, `docs/adr/0027-embedding-identity-on-chunks.md`

**Done when.**
- Seed 5 legacy documents: 3 reproducible by model A and 2 by model B. After an upload with model A, B's rows remain NULL (or stranded) and are never stamped A.
- The stamp writes an audit entry naming the proven documents.
- Take a legacy model-B corpus whose forced re-index to model A stopped after converting 3 documents. An unforced restart never stamps the unconverted B rows as A.

**ADR.** ADR-0027 §3 specifies the sample-proven org-wide stamp. This item narrows it, and §3 needs an amendment.

### R-knowledge-15

**GET /knowledge resolves the viewer's embedder (key store, policy, registry) and aggregates the org's chunks on every list request** · tier 3

**Problem.** `ListDocuments` calls `viewerIdentity`, which runs the full `embedderFor` resolution, then `CountStrandedKnowledgeChunksByDocument`, a `GROUP BY` over all of the org's chunks. This happens on every list request. Errors are swallowed to a WARN that blanks the stranded badges. The panel requests the whole list with no limit (`KnowledgeBasePanel.tsx:94`). It refetches after every upload (`:131`) and delete (`:163`), and once when a re-index ends (`:237`), is stopped (`:245`) or fails after committing pages (`:254`). The page loop at `:217-223` makes no list call. A large org therefore pays credential reads and a chunk-table aggregate on every list request.

**Evidence.**
- `internal/rag/service.go:611` — `ListDocuments` calls `viewerIdentity` (616; `:633-643` runs `embedderFor` at 638), then `CountStrandedKnowledgeChunksByDocument` (620); an error is logged as WARN and the documents are returned without stranded counts (621-623)
- `internal/storage/database/postgres_knowledge.go:715` — `GROUP BY c.doc_id` over the org's chunks of another identity (715-720)
- `frontend/src/components/settings/KnowledgeBasePanel.tsx:94` — the list request carries no limit
- `frontend/src/components/settings/KnowledgeBasePanel.tsx:131` — `loadDocs` after upload; `:163` after delete; `:237` at the end of a re-index, `:245` on stop, `:254` on failure when `pages > 0`; none inside the page loop (217-223)

**Change.** Memoise the viewer identity per (scope, org) for a minute under a `cache.Declare` spec, invalidated on credential or policy changes. Memoise the stranded-by-document map per (org, identity) under the existing corpus invalidation. Alternatively, compute stranded counts in the LATERAL list statement with the identity as a parameter, so one statement serves the request. The cost is per list request, not per re-index page.

**Touches.** `internal/rag/service.go`, `internal/rag/search_cache.go`, `internal/storage/database/postgres_knowledge.go`, `internal/di/testdata/caches.golden`

**Done when.**
- Two list calls within the TTL perform one embedder resolution (recording factory) and one stranded aggregate.
- An upload invalidates the stranded map, so the next list reflects it.
