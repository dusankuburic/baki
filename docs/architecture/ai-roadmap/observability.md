# AI roadmap: Observability and operations

Items of category `observability` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-observability-10

**Traces cannot reconstruct a turn: no turn span, retries collapse into one provider span, and tools, approvals, RAG and persistence are unspanned** · tier 2

**Problem.** The otelhttp span for `POST /api/chat/stream` ends when the handler returns the stream id, within milliseconds. The worker keeps that span context through `context.WithoutCancel`, so the provider spans land under a parent that has already ended. No span describes the turn itself (stream id, flow, outcome, iterations). `TracedProvider` wraps `RetryingProvider`, so three attempts with 30 s of backoff show up as one `<provider>.Stream` span, and nothing explains the waits inside its duration. `pad_ai_retry_attempts_total` already counts retries as a metric (`internal/ai/retry.go:153`, `:188`, `:246`); what is missing is seeing the attempts inside one trace. These steps have no spans: context build, each tool execution (timed at `chat_tool_exec.go:72-75` but not spanned), the approval wait, the fix apply itself (`chat_fixes.go:325`) and the answer persist. The 800 ms RAG lookup has no enclosing span either. Its query embed already produces a `<provider>.Embed` span through the factory chain (`internal/ai/tracing.go:47-59`), but the corpus count, the membership check and the vector query time are not visible. The re-analysis after an approved fix is already spanned: `applyApprovedFix` calls `s.analysisCache.AnalyzeFlow` (`chat_fixes.go:339`), which reaches the `AnalyzeFlow` span (`analysis.go:350-351`, `:413`). That span is the precedent for the rest.

**Evidence.**
- `internal/service/chat.go:704` — the worker ctx is `WithoutCancel(request ctx)`, so its span context comes from the HTTP span, which ends when `internal/api/handlers_chat.go:89` renders the id
- `internal/ai/factory.go:264` — the chain is breaker, then retry, then Traced, then audited (264-267), so the trace span sits outside the retry loop
- `internal/ai/tracing.go:61` — Stream span attributes are `ai.provider`, `ai.model` and tokens (61-90); there is no stream id or attempt
- `internal/service/chat_tool_exec.go:72` — tool execution is timed (72-75) but has no span
- `internal/service/analysis.go:413` — precedent: `otel.Tracer("analysis-service").Start(ctx, "AnalyzeFlow")`
- `internal/service/chat.go:937` — `provider.Stream` and the tool loops run with no enclosing turn span

**Change.** Start a `chat.turn` span in `StreamChatMessage`'s worker. Make it a new root span linked to the HTTP span (`trace.WithLinks`), not a child of it, so the stream is never shown as a child that outlives its parent. Its attributes are stream id, flow id, provider, model, use_tools, outcome code, iterations, tokens, cost and served model, and it ends in the deferred cleanup. Add child spans for context build, `rag.lookup` (corpus count, membership, vector query time), each tool call (`tool.<name>` with name, ok, bytes), `approval.wait` (status), `fix.apply` and `conversation.persist`. To show attempts, have `RetryingProvider` add an attempt event with reason and backoff to the current span (`ai.retry_attempts`). Prefer this to moving `TracedProvider` inside the retry wrapper: that would change what the breaker and audited layers see, and every provider chain shares the factory order (`factory.go:264-267`). Set span status Error for every `ChatErr*` terminal except `STREAM_STOPPED`.

**Touches.** `internal/service/chat.go`, `internal/service/chat_tool_exec.go`, `internal/service/chat_approvals.go`, `internal/service/chat_context.go`, `internal/ai/tracing.go`, `internal/ai/retry.go`, `internal/ai/tracing_test.go`

**Done when.**
- With an in-memory span exporter, one `StreamChatMessage` with two tool iterations and one approval produces a `chat.turn` span with child spans `<provider>.Stream` ×2, `tool.<name>` ×N and `approval.wait` ×1, all carrying the stream id.
- A retried stream shows the attempt count and backoff as span events (`ai.retry_attempts=2` for two attempts).
- Span status is Error for every `ChatErr*` terminal except `STREAM_STOPPED`.

### R-observability-3

**usage_metrics rows carry no stream id, call kind, cache slice or provenance, and a crashed replica writes none** · tier 3

**Problem.** A usage row stores user, org, provider, model, `served_model`, prompt and completion tokens, cost and time. A tool-loop turn writes one row per provider iteration, an embed writes a row under the embedding model's name, and a connection test writes another. No row carries any of the following:
- the stream id, conversation key or flow id
- the call kind (chat, stream, embed or probe)
- the request id
- whether the numbers were provider-reported or cl100k estimates (the post-stream fallback, cancelled turns, compat servers)
- the cache read and write slices the price was computed from

An operator reconstructing what one chat request cost has to guess by timestamp, and a cost spike cannot be attributed to a conversation or feature. Rows are queued in-process on a bounded runner when each provider call ends: at its Done (`audited.go:252-253`) or at the end of the call (`:285-296`). A replica that dies mid-stream therefore loses the in-flight call's row and any saves still queued on `usageRecords`; earlier iterations' rows are normally already written. Its Redis ledger entry simply lapses after the 30 s lease, and `GetDailyUsage` sums persisted rows only. The day's recorded spend is then lower than the provider bills, and the gap leaves no trace beyond `pad_ai_ledger_expired_total`.

**Evidence.**
- `internal/storage/interfaces/usage.go:15` — `UsageMetric` fields (15-26): no stream, conversation or flow id, no kind, no cache tokens, no request id, no source flag
- `internal/storage/database/postgres_users.go:1003` — the INSERT columns match the struct: `id, user_id, org_id, provider, model, served_model, prompt_tokens, completion_tokens, estimated_cost, created_at`
- `internal/ai/audited.go:523` — cache read and write tokens change the price (523-526), but only `u.In` and `u.Out` are stored (543-544); the estimate fallback (285-296) writes the same row shape as a reported one
- `internal/ai/audited.go:252` — each Stream call records its own row, so a 6-iteration tool loop gives up to 6 unlinked rows; Embed rows (632-667) differ from chat rows only by model name
- `internal/ai/audited.go:421` — `record` queues the save on `usageRecords` (`lifecycle.Bounded`, 442) with a 10 s save (501-509); nothing is written before the call ends
- `internal/redisx/inflight.go:62` — `inflightLease` is 30 s, so a dead replica's entry stops counting; `pad_ai_ledger_expired_total` counts the orphans (77-80); `GetDailyUsage` (`postgres_users.go:1018-1026`) sums persisted rows only
- `internal/ai/audited.go:248` — a Done with zero usage: `tokensOut` and `tokensIn` are replaced by estimates (248-251), then go through the same `recordAITokens` and `record` (252-253)
- `internal/ai/audited.go:285` — no Done: estimates are substituted (286-291) and recorded through the same path (294-295)

**Change.** Ship an expand migration that adds nullable columns to `usage_metrics`, with an index on `(stream_id)`:
- `stream_id`
- `call_kind` (`chat|stream|embed|probe|knowledge`)
- `cache_read_tokens` and `cache_write_tokens`
- `request_id`
- `usage_source` (`'reported'|'estimated'|'held'`)
- `finish_reason`

Carry the stream id and call kind on the context and read them in `auditedProvider.usageMetric`. The chat worker already stamps a budget scope at `chat.go:715`; add a usage-attribution value beside it. Write a provisional "started" row (or a Redis marker beside the reservation) at call start and finalise it on completion, so a crash leaves a visible estimate. Add a reconciliation job that converts expired ledger entries into estimated rows. Add an admin query (`GET /api/admin/usage?streamId=|userId=&from=`) and surface `usage_source` in the analytics endpoint.

**Touches.** `internal/storage/interfaces/usage.go`, `internal/storage/database/migrations/`, `internal/storage/database/postgres_users.go`, `internal/storage/database/testdata/migration_checksums.golden`, `internal/ai/audited.go`, `internal/ai/budget.go`, `internal/service/chat.go`, `internal/service/ai_budget_redis.go`, `internal/rag/service.go`, `internal/api/handlers_admin.go`

**Done when.**
- A tool-loop turn with 3 iterations writes 3 usage rows that share one `stream_id` and have `call_kind=stream`.
- A RAG query embed during the same turn writes a row with the same `stream_id` and `call_kind=embed`.
- A Stream that ends through the estimate fallback writes `usage_source='estimated'`.
- A Claude Done with `cache_read` 500 writes `cache_read_tokens=500`.
- `SELECT SUM(estimated_cost) FROM usage_metrics WHERE stream_id=$1` equals the cost implied by the done event's rates and tokens (tested over the fake backend).
- After a simulated replica crash (ledger entry past its lease, no row), the reconciliation job writes an estimated row for the reservation.
- The migration ships as expand with a checksum golden refresh, and the `GetDailyUsage` query plans are unchanged.

### R-observability-6

**Failed uploads and first-page re-index failures leave no audit row, and a failed upload still invalidates the corpus cache on every replica** · tier 3

**Problem.** `handleKnowledgeUpload` records `audit.ActionKnowledgeUpload` only after `AddDocument` succeeds. `handleKnowledgeReindex` audits a failure only when pages were committed (`res.NextCursor != nil`). A refused or failed attempt that already paid for embedding calls is therefore invisible in the audit trail.

Separately, once `AddDocument` holds the org lease it publishes `KnowledgeDocChanged` from a defer, whatever the outcome (`service.go:521-531`). Failures before the lease publish nothing (`service.go:495-524`): empty content, an embedder or budget refusal, a document that is too large, `KNOWLEDGE_BUSY` and lock-unavailable. Failures after the lease do publish (`:535-594`): the supersede lookup, the 409 model-changed refusal, an embedding failure, a count mismatch and a save failure. Each published event bumps the corpus-count fence and sets the 10 s "non-empty" settle marker. That happens on this replica and, through the replicated invalidation, on every other one. For an org with an empty corpus the model guard never runs (`:543`), so there the wasted case comes from embedding or save failures. During the window every chat turn on that org pays for a query embedding for nothing, and a burst of failed uploads keeps the window open.

**Evidence.**
- `internal/api/org_knowledge.go:93` — `audit.Record` runs only after `AddDocument` returned nil (`:101`)
- `internal/api/org_knowledge.go:307` — a re-index failure is audited only when `res.NextCursor != nil`; nothing is audited otherwise (`:311-315`)
- `internal/rag/service.go:527` — the deferred `Publish(KnowledgeDocChanged)` runs "whatever the outcome" (529-531); it is registered after `lockOrg` (521-525)
- `internal/rag/service.go:543` — the model guard runs only when `len(superseded) < total`, never for an empty corpus
- `internal/rag/service.go:575` — `docID` is assigned just before `SaveKnowledgeDocumentWithChunks` (593)
- `internal/rag/search_cache.go:84` — `InvalidateCorpus` sets `corpusUnknown` for `corpusSettleTTL` (10 s, line 21)
- `internal/rag/search_cache.go:98` — the subscriber is `Replicated`, so the invalidation reaches every replica (100-102)

**Change.** Audit every upload and re-index attempt with its outcome and machine code. For cost reconciliation, point at the existing usage rows rather than adding chunk counts to audit metadata. Do not publish only on `err == nil`: a lease lost after the commit (`lostOr`) or an ambiguous commit error would then skip a real invalidation. Either publish whenever `docID` was assigned and the save was attempted (`service.go:575`, `593` onward), so failures before the save publish nothing, or keep the unconditional publish and accept the 10 s settle window.

**Touches.** `internal/api/org_knowledge.go`, `internal/rag/service.go`, `internal/rag/search_cache.go`, `internal/rag/search_cache_test.go`, `internal/api/handlers_org_knowledge_test.go`

**Done when.**
- A 409, 429 or 503 upload leaves an audit row with `failed=true` and the code, and a first-page re-index failure does the same.
- An `AddDocument` on an empty-corpus org that fails at embedding publishes no `KnowledgeDocChanged` (checked on a recorded bus), and the next Search makes zero Embed calls.

### R-observability-11

**Operators have no surface for AI subsystem state: admin health omits it and streams cannot be listed or cancelled by an admin** · tier 3

**Problem.** `/api/admin/system/health` reports only database, blob and Redis. Nothing reports:
- whether the budget ledger is failing closed
- how many usage rows were dropped
- which providers have an open breaker
- how many streams are live, and for how long

`chat_stream_active` is a bare count, and no endpoint lists active streams. `streamCtl` holds owner, org, flow, conversation key, in-flight cost and `lastActivity` (`chat_stream_control.go:100-131`). It does not hold provider, requested model, start time or iteration count, so a listing needs those fields added to `streamCtl`. The cancel route refuses anyone but the owner. Until the 10-minute wall-clock cap (`chat.go:36`, `704`) or the 90 s idle watchdog (`chat_resume.go:50`, `230`) fires, only its owner can stop a runaway or stuck stream. Breaker state is a transitions counter, a log line and, since step 6, the `pad_ai_circuit_breaker_open` gauge.

**Evidence.**
- `internal/api/handlers_system.go:479` — `adminHealthResponse`: `Database`, `Blob`, `Redis`, `Overall`, `deployInfo` (479-485); no AI component
- `internal/api/handlers_chat.go:301` — `ownsStreamForCommand` (301-315): 403 `STREAM_NOT_FOUND` unless the owner is the caller
- `internal/service/metrics.go:13` — `chat_stream_active` is only a count; `streamCtl` (`chat_stream_control.go:77-152`) holds owner, org, flow, conversation key, in-flight cost and `lastActivity` (100-131), and nothing exposes them
- `internal/service/chat.go:36` — `maxChatStreamDuration` = 10 min, applied at 704; `chat_resume.go:50` `streamIdleTimeout` = 90 s, enforced at 230-232
- `internal/ai/circuitbreaker.go:177` — the exported API is the constructors; state is visible only through `recordCircuitBreakerTransition` (346) and the Warn line (350)
- `internal/ai/metrics.go:11` — `ai_circuit_breaker_transitions_total` is a counter; there is no "open" gauge

**Change.** The cheap, useful part is an `ai` component on `adminHealthResponse`. It reports ledger ok or degraded from the last hold or read error, usage recorder drops since boot, open breakers with provider id, the active stream count and the oldest start. The oldest start needs a start time added to `streamCtl`. Admin routes are optional. `GET /api/admin/chat/streams` would list this replica's active streams: id, owner, flow, provider, model, started, `lastActivity`, iterations and in-flight cost. `POST /api/admin/chat/streams/{id}/cancel` would be audited, admin-only and relayed across replicas through the existing command bus. Both routes need provider, model, start time and iteration count on `streamCtl`, the route-policy and authz goldens, and audit wiring. Streams are already bounded by `maxChatStreamDuration` and the idle watchdog, so an admin cancel is a convenience, not the only remedy. Tier 3.

**Touches.** `internal/api/handlers_system.go`, `internal/api/handlers_admin.go`, `internal/service/chat_commands.go`, `internal/service/chat.go`, `internal/service/chat_stream_control.go`, `internal/ai/circuitbreaker.go`, `internal/api/testdata/route_policy.golden`, `internal/api/testdata/authz_matrix.golden`

**Done when.**
- Admin health lists `ai.ledger`, `ai.usageDrops`, `ai.breakersOpen` and `ai.activeStreams`, and a forced ledger error flips `ai.ledger` to error.
- If the admin routes are built:
  - `GET /api/admin/chat/streams` shows a running test stream with owner and provider.
  - The admin cancel ends it with `STREAM_STOPPED` and writes an audit row.
  - A member calling it gets 403.
- The route-policy and authz-matrix goldens are refreshed for any new admin routes.

### R-observability-13

**Alert runbook names no AI-provider health metric; no procedure for an open circuit or a retry storm** · tier 3

**Problem.** The alert list in `operations.md` names `pad_ai_ledger_errors_total` and `pad_usage_dropped_total`. It does not name any of these, although all are registered and pinned in `metrics.golden`:
- `ai_circuit_breaker_transitions_total`
- `pad_ai_retry_attempts_total`
- `pad_ai_budget_refusals_total`
- `pad_ai_pricing_fallback_total`
- `pad_chat_tool_refused_total`
- `pad_chat_fix_decisions_total{status="timeout"}`

The only runbook mention of breakers says that they are per replica. Users can get `PROVIDER_UNAVAILABLE` ("temporarily unavailable after repeated failures"). The operator then has no documented signal to alert on and no steps to follow: which provider and model, per replica, the 30 s cooldown, how to confirm upstream status, and when to lower `PAD_CB_FAILURES` or switch provider. `ai-providers.md:24` documents the budget-refusal metric, but not as an alert.

**Evidence.**
- `docs/architecture/operations.md:132` — alert list: the only AI families are `pad_ai_ledger_errors_total` and `pad_usage_dropped_total`
- `docs/architecture/operations.md:126` — "Per replica on purpose: … provider circuit breakers" is the only match for circuit, breaker or `PROVIDER_UNAVAILABLE`
- `internal/metrics/testdata/metrics.golden:1` — `ai_circuit_breaker_transitions_total` is registered, as are `pad_ai_budget_refusals_total` (11), `pad_ai_pricing_fallback_total` (16), `pad_ai_retry_attempts_total` (17), `pad_chat_fix_decisions_total` (45) and `pad_chat_tool_refused_total` (49)
- `internal/service/chat_errors.go:247` — `ErrCircuitOpen` maps to the `PROVIDER_UNAVAILABLE` copy the user sees (247-248)
- `internal/ai/circuitbreaker.go:350` — the transition is logged as WARN "AI circuit breaker transition" with provider, state and failures, which a log alert can match; the counter at 346 carries provider and state only
- `internal/ai/circuitbreaker.go:138` — the breaker is keyed per `provider|model` (136-143, 169-175), but the model never reaches the log line or the metric
- `docs/architecture/ai-providers.md:24` — `pad_ai_budget_refusals_total{reason="unavailable"}` is documented as a metric, not as an alert

**Change.** Add an "AI providers" runbook section with these alerts:
- `increase(ai_circuit_breaker_transitions_total{state="open"}[15m]) > 0`
- `rate(pad_ai_retry_attempts_total[5m])` above a per-provider baseline
- `pad_ai_budget_refusals_total{reason="unavailable"} > 0`
- sustained `pad_ai_pricing_fallback_total`
- `pad_chat_fix_decisions_total{status="timeout"}`

Add a bicep log alert `ai-circuit-open` on the WARN line. Each signal gets steps: identify the provider and model from the log line, check upstream status, review `PAD_CB_OPEN_DURATION` and `PAD_CB_FAILURES`, and switch provider. The model is not on the line today, so add it to the WARN line in `transitionLocked` first (not to the counter), or drop the model from that step. Add a test that every `internal/ai` family in `metrics.golden` is named in `operations.md`.

**Touches.** `docs/architecture/operations.md`, `infra/main.bicep`, `internal/metrics/metrics_docs_test.go`, `internal/ai/circuitbreaker.go`

**Done when.**
- A test reads `internal/metrics/testdata/metrics.golden`, selects the families owned by `internal/ai`, and fails if any name is absent from `docs/architecture/operations.md`.
- When `alertEmails` is set, `infra/main.bicep` provisions an `ai-circuit-open` log alert matched on the "AI circuit breaker transition" line with state open.
- The runbook section names the user-facing code (`PROVIDER_UNAVAILABLE`) that each signal corresponds to.

### R-observability-17

**Key-rotation re-seal is observable only through log lines and finishes only across restarts** · tier 3

**Problem.** The re-seal runs once per boot under an advisory lock with a 30-minute ceiling. "Remaining" and "unreadable" counts appear only as a warn line, and the ADR tells operators to restart a replica or add the key. No metric exposes the counts, and the job metrics report success even when values remain. `pad_job_runs_total{job="secret_reseal",outcome}` and `pad_job_last_success_timestamp_seconds` are recorded for this job (`lifecycle/singleton.go:393`; `metrics.golden:64-68`). `Run` returns nil whenever no pass errored (`jobs_reseal.go:94-95`), so a pass that leaves remaining or unreadable values is recorded as `outcome=ok`, a "last success". No health field or admin endpoint exposes the report either. Operators cannot verify that a rotation completed across replicas without grepping logs, and previous keys stay in the environment indefinitely.

**Evidence.**
- `internal/app/jobs_reseal.go:44` — `scheduleSecretReseal`: runs once per boot and only with previous keys (49-51); `secretResealTimeout` is 30 min at 35
- `internal/app/jobs_reseal.go:104` — `logSecretReseal`: counts and unreadable refs go only to the log (105-122)
- `internal/app/jobs_reseal.go:94` — `Run` reports and returns nil after the passes whatever the counts (94-95)
- `internal/lifecycle/singleton.go:393` — `RecordJobRun(s.name, outcome, took)`: outcome is ok unless the run errored, panicked or lost its lease (384-392)
- `docs/adr/0005-secret-columns-encrypted-at-rest.md:13` — the operator procedure keys on the log line: "`remaining` > 0: restart one replica"
- `internal/metrics/testdata/metrics.golden:64` — only the generic job families (64-68); no reseal or keystore family

**Change.** Add `pad_secret_reseal_remaining` and `pad_secret_reseal_unreadable` gauges through `metrics.golden`, and optionally `checked` and `resealed`. Do not add a last-run timestamp: `pad_job_last_success_timestamp_seconds{job="secret_reseal"}` and `pad_job_runs_total` already exist. Add the report to `/readyz` details or to `GET /api/admin/system/keys` beside the admin health handler (`handlers_system.go:504`). That route is admin-only and returns counts and refs, never values. Add `bakicli keys reseal-status`. To re-run while values remain, set `Job.Every` (`lifecycle/singleton.go:35`) on the existing Singleton instead of adding a separate timer. The job exists only while previous keys are configured (`jobs_reseal.go:49-51`), so the interval applies only then.

**Touches.** `internal/app/jobs_reseal.go`, `internal/metrics/`, `internal/api/handlers_system.go`, `cmd/bakicli/`, `docs/adr/0005-secret-columns-encrypted-at-rest.md`

**Done when.**
- The metrics golden gains the reseal series. After a pass with one unreadable row, the gauge reads 1 and the admin endpoint lists the row's table, column and row id.
- A pass interrupted at the 30-minute ceiling is resumed by the job's interval without a restart (tested with a short ceiling).

**ADR.** ADR-0005 documents the log-line procedure only; no ADR specifies a metric or endpoint.
