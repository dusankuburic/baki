# AI roadmap: Observability and operations

Items of category `observability` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-observability-1

**A chat turn leaves no terminal record: outcome, duration, tokens, cost and tool/approval counts are neither logged nor metered** · tier 1

**Problem.** The only audit row for a turn is written at accept (`chat.stream`, Meta `{streamId}`), before the worker runs. Some parts are already recorded:
- Tokens and cost are metered per provider call (`ai_tokens_total`, `pad_ai_usage_estimated_cost_total`) and persisted per call in `usage_metrics`.
- Tool executions and approval outcomes are metered in aggregate (`pad_chat_tool_results_total{tool,ok}`, `pad_chat_fix_decisions_total{status}`).
- A successful turn with history saving on is stored with tokens, served model, rates and tool `durationMs`.

The turn itself is not. The worker emits `done` or `error` to the client and exits with no log line and no counter. There is no `pad_chat_turns_total{outcome}`, no turn-duration histogram, and no approval-wait or tool-duration histogram. Idle-watchdog and SSE-gone cancellations are not counted (Info log only). Both also cancel through `cancelWithReason` with an empty code, so `classifyFailureBase` reports them as `STREAM_STOPPED`, the same as a user stop. The failure codes are already a closed set in `chat_errors.go`, but they feed no metric. An operator asked what happened to a user's chat at 14:03 can find the accept row and the per-call usage rows, but no outcome. And "how many turns failed with `PROVIDER_RATE_LIMITED` this hour" is unanswerable.

**Evidence.**
- `internal/api/handlers_chat.go:88` — `audit.ActionChatStream` is recorded on accept with Meta `{streamId}` only, before any outcome exists.
- `internal/service/chat.go:981` — `done` is emitted with tokens and rates; no logger call, no counter.
- `internal/service/chat.go:993` — the error path is `classifyFailureShared` + `setFailure` + `emit("error")` (993-1003); no log, no metric.
- `internal/service/chat.go:1543` — `failStream` and `failUsageStream` (1543-1562) only call `setFailure` and emit.
- `internal/service/chat.go:810` — the worker's deferred cleanup (810-836).
- `internal/service/metrics.go:13` — the `chat_stream_active` gauge is the only turn-level metric (`chatStreamStart`/`End` at 97-98). Tool results and fix decisions are counted in aggregate (39-52).
- `internal/service/chat_resume.go:231` — the idle-watchdog cancellation is logged at Info (SSE-gone at 249). Both cancel through `cancelWithReason` (232, 250) with an empty code (`chat_stream_control.go:272`), which `classifyFailureBase` reports as `STREAM_STOPPED` (`chat_errors.go:231-232`).
- `internal/ai/metrics.go:18` — `ai_tokens_total` per provider call; `pad_ai_usage_estimated_cost_total` at 72-77.
- `internal/ai/audited.go:533` — each call's `usage_metrics` row carries user, org, tokens and cost (533-547).
- `internal/service/chat_conversation.go:242` — a stored answer carries tokens, served model and rates (242-258); tool `durationMs` is stored at 303.

**Change.**
- In the worker's deferred cleanup (`chat.go:810-836`), emit one structured `chat turn ended` line through `logger.FromContext(ctx)`. It carries stream_id, flow_id, conversation key, provider, requested and served model, outcome code, tokens in/out, billed rates, iterations, tool calls, approvals by status and duration.
- Register these metrics and refresh the metrics golden:
  - `pad_chat_turns_total{outcome}`: `done` plus each `ChatErr*` code, with `idle_timeout`, `subscriber_gone` and `stopped` distinct from provider failures.
  - `pad_chat_turn_duration_seconds`, with a top bucket at or above `maxChatStreamDuration`.
  - `pad_chat_tool_duration_seconds{tool}`.
  - `pad_chat_approval_wait_seconds{status}`.
- Distinct outcome labels need distinct cancel codes. The idle and subscriber-gone cancellations at `chat_resume.go:232` and `:250` must call `cancelWithCode` with their own codes instead of `cancelWithReason`.
- Time-to-first-token at the provider layer is [R-observability-8](observability.md#r-observability-8).

**Touches.** `internal/service/chat.go`, `internal/service/metrics.go`, `internal/service/chat_resume.go`, `internal/service/chat_tool_exec.go`, `internal/service/chat_approvals.go`, `internal/api/handlers_chat.go`, `internal/metrics/testdata/metrics.golden`

**Done when.**
- Every stream exit path logs exactly one `chat turn ended` line with stream_id, request_id, user_id, provider, model, outcome, tokens in/out and duration. That covers `done`, every `ChatErr*` code, `ensureTerminal` and a shutdown-forced end.
- `pad_chat_turns_total{outcome}` increments once per stream, with the outcome the client saw. `idle_timeout`, `subscriber_gone` and `stopped` are distinct label values, and `metrics.golden` is updated.
- The turn-duration histogram's top finite bucket is >= `maxChatStreamDuration` (600 s).

### R-observability-2

**Chat and AI log lines bypass logger.FromContext, so none carries request_id, user_id or trace_id** · tier 1

**Problem.** `docs/architecture/README.md:32` sets the convention: request-path logs go through `logger.FromContext(ctx)` so they carry `request_id`, `user_id` and `trace_id`. The detached worker context (`WithoutCancel`) still carries `reqinfo.Info` and the span context. But every `chat_*.go` and `internal/ai` log call uses the global `logger.Warn`/`Info`/`Error`; there are zero `FromContext` uses in either tree. A few lines carry part of the identity: `chat_errors.go:128` and `:150` log the caller id under the key `scope`, and `chat.go:802` and `chat_resume.go:231`/`249` carry `streamID`. None carries `request_id` or `trace_id`, and none uses the `user_id` key. Breaker transitions, usage drops, pricing fallbacks, budget-check failures, provider setup failures, idle kills and the upstream-request-id lines therefore cannot be joined to the trace. Nor can they be joined reliably to the access-log line of `POST /api/chat/stream` or to the user. In the OpenAI-compatible family, "AI upstream error" is logged only when the vendor sent a request-id header (`logUpstreamRejection`). So a vendor support ticket cannot be tied back to the baki user or stream that hit it.

**Evidence.**
- `docs/architecture/README.md:32` — the convention: request-path logs go through `logger.FromContext` so they carry `request_id`/`user_id`/`trace_id`.
- `core/logger/context.go:29` — `FromContext` is the only path that attaches the extractor's attributes; `slog.Default()` lines carry none.
- `internal/api/middleware/request_info.go:71` — `LogAttrs` yields `request_id`, `user_id` and `trace_id` from the ctx.
- `internal/service/chat.go:704` — `context.WithoutCancel` keeps the values, so the worker ctx can still feed `FromContext`.
- `internal/service/chat.go:590` — a global `logger.Warn`. The same pattern is at 191, 802 and 910, `chat_errors.go:128` and 150, `chat_resume.go:231` and 249, and `chat_fixes.go:371` and 642. A grep finds no `logger.FromContext` in `chat*.go` or `internal/ai`.
- `internal/api/security.go:100` — `KeyScope` is the caller id (100-105), which `chat_errors.go:128` and 150 log under `scope`.
- `internal/ai/openai_compat.go:721` — `logUpstreamRejection` returns before logging when there is no upstream request id (722-725, within 719-731). `audited.go` 458/488/506/518 log with provider and model only.

**Change.**
- Route every `chat_*.go` and `internal/ai` log call that has a ctx through `logger.FromContext(ctx)`. The audited wrapper, retry, breaker `Stream`/`Chat` and the raw providers all receive a ctx.
- Add `stream_id` to the worker ctx through a small value key, or extend `reqinfo.Info` with a stream field, so every line under the turn carries it.
- Always log a non-2xx upstream response at Warn, with status, provider, model and the upstream request id when present, rather than only when a request-id header exists.
- Add an archtest ratchet pinning the count of bare `logger.*` calls in `chat*.go` and `internal/ai`.

**Touches.** `internal/service/chat.go`, `internal/service/chat_errors.go`, `internal/service/chat_resume.go`, `internal/service/chat_fixes.go`, `internal/service/chat_conversation.go`, `internal/service/chat_conversation_version.go`, `internal/ai/audited.go`, `internal/ai/openai_compat.go`, `internal/ai/circuitbreaker.go`, `internal/archtest`

**Done when.**
- A test drives one `StreamChatMessage` through a failing fake provider and asserts that every emitted log line carries `request_id`, `user_id`, `stream_id`, and `trace_id` when a span is active.
- A 503 from a stub upstream without any request-id header produces exactly one "AI upstream error" line with status and provider.
- An archtest pins the count of bare `logger.*` calls in `chat*.go` and `internal/ai`, and the count only decreases.

### R-observability-5

**The 800 ms chat RAG budget is unobservable: no latency metric and deadline misses share the 'skipped' label with the membership gate** · tier 1

**Problem.** `prepareTurn` runs Search under a fixed 800 ms context. Inside that window Search does the following:
1. A cached-or-DB chunk count.
2. Resolving the embedder (key store, policy, registry).
3. A budget check. Inside a chat turn this reads the turn's memo, not a fresh read.
4. An embed over the network, through a retry wrapper whose backoff gives up only when the deadline cannot cover the wait.
5. The vector query.
6. On a miss, another identity census.

When the window expires during Search, `ragGuidelines` records the outcome `skipped`, the same label the non-member gate uses. A deadline that expires inside `explainEmptySearch` is swallowed and recorded as `miss`. A caller without an embedder is recorded as `error`. There is no histogram of where the time went. Under a slow provider most turns silently lose guidelines, and the operator cannot tell policy from latency. `pad_chat_rag_lookups_total` is the only RAG metric in `service/metrics.go`. The other RAG signals are `pad_rag_dim_mismatch_total` and `pad_rag_stranded_search_total`. The query embed appears in `ai_request_duration_seconds{provider}`, mixed with every other call, and as a `<provider>.Embed` span.

**Evidence.**
- `internal/service/chat.go:558` — `ragCtx` is derived with `ragGuidelinesDeadline`.
- `internal/service/chat_context.go:33` — `ragGuidelinesDeadline = 800 ms`, a constant, not configurable.
- `internal/service/chat_context.go:232` — a non-member records `"skipped"`. A context expiry records the same `"skipped"` at :242. A Search error with a live context, including no embedder, records `"error"` at :240.
- `internal/service/metrics.go:81` — `pad_chat_rag_lookups_total` is the only RAG metric in this package (a counter by outcome).
- `internal/rag/metrics.go:10` — `pad_rag_dim_mismatch_total` and `pad_rag_stranded_search_total` (10-25).
- `internal/rag/service.go:668` — `embedderFor` runs per Search. The `checkBudget` at :674 reads the turn's memo inside a chat turn (672-673; `chat.go:715`, `service.go:150-155`). The extra `CountKnowledgeChunksByIdentity` on a miss is at :726, and its error is swallowed (726-728).
- `internal/ai/retry.go:86` — backoff returns `lastErr` only when the deadline cannot cover the wait.
- `internal/ai/audited.go:637` — the query embed is observed in `ai_request_duration_seconds{provider}` alongside every other call (637-639), and spanned as `<provider>.Embed` (`factory.go:267`, `tracing.go:47`).
- `internal/service/providers.go:28` — `providers.model-listings` is declared `cache.Shared` with `MechanismInvalidation` (28-30).

**Change.**
- Split the outcome labels into `non_member`, `no_embedder`, `deadline`, `error`, `miss` and `hit`. `deadline` splits today's `skipped`, and `no_embedder` splits today's `error`: `ErrEmbeddingNotConfigured` is currently recorded as `error`, so the split also removes noise from the error series.
- Add `pad_chat_rag_lookup_seconds` with a `phase` label (`resolve`, `embed`, `search`).
- Make the deadline configurable as `PAD_RAG_DEADLINE_MS`, validated in `internal/config`, with an 800 ms default.
- Memoise the resolved embedder per (scope, org) under a `cache.Declare`. The memo holds a credentialed provider chain, so a TTL alone is not enough. It needs the same credential and policy invalidation that `providers.model-listings` declares; otherwise a revoked key or a newly disallowed provider would keep embedding for up to a minute.
- Move `explainEmptySearch`'s census off the turn's critical path: run it after `ragDone` is delivered, best effort.
- Refresh the metrics and cache goldens.

**Touches.** `internal/service/chat_context.go`, `internal/service/chat.go`, `internal/service/metrics.go`, `internal/metrics/testdata/metrics.golden`, `internal/rag/service.go`, `internal/rag/search_cache.go`, `internal/config/config.go`, `internal/config/loader.go`

**Done when.**
- A Search that sleeps past the deadline yields guidelines `''` and `pad_chat_rag_lookups_total{outcome="deadline"}`. A non-member turn records `outcome="non_member"`.
- The metrics golden lists `pad_chat_rag_lookup_seconds` with the `phase` label.
- Two Search calls within the memo TTL for the same (scope, org) perform one key-store read, counted through the recording factory.

### R-observability-7

**Breaker observability cannot name the model or Azure resource that is open, and there is no open-circuit gauge** · tier 2

**Problem.** Circuit state is keyed `providerID[#resource]|model`, but the transition metric and the log line carry only the provider id, and `ErrCircuitOpen` carries no key. Nothing shows how many circuits are open right now or for how long. Circuits are per model and per tenant resource, yet an operator sees only "azure-foundry opened", without knowing which deployment or whose resource. The operator cannot alert on "open for more than N minutes", and the user sees the generic "temporarily unavailable after repeated failures". For a harness that depends on one deployment, an outage then looks the same as a misconfiguration.

**Evidence.**
- `internal/ai/circuitbreaker.go:160` — "scope ... only keys the state; metrics and logs keep providerID" (160-162)
- `internal/ai/circuitbreaker.go:341` — `transitionLocked` logs and counts only `cb.providerID` and the state (346-350)
- `internal/ai/metrics.go:11` — the `ai_circuit_breaker_transitions_total` labels are provider and state; this file has no gauge of open circuits
- `internal/ai/provider.go:208` — `ErrCircuitOpen` is a bare sentinel with no key
- `internal/service/chat_errors.go:247` — the chat code for `ErrCircuitOpen` is a fixed generic message

**Change.** Add two labels to the transition counter and the log line. The first is a bounded model label: the request model for catalogue providers and `deployment` for alias providers, as `pricingMetricModel` does. The second is a short non-reversible resource hash. `circuitbreaker.go:124` documents that the scope "holds no secret and never reaches logs", so the log line must use the same hash as the metric label, never the raw endpoint. Add a gauge `pad_ai_circuit_breaker_open{provider}` maintained in `transitionLocked`; [R-observability-11](observability.md#r-observability-11)'s admin health reads this gauge. Return a typed `circuitOpenError{provider, model, scope, retryAfter}` that unwraps to `ErrCircuitOpen`, so the chat service can say which deployment failed and when to retry. Refresh the metrics golden.

**Touches.** `internal/ai/circuitbreaker.go`, `internal/ai/metrics.go`, `internal/ai/provider.go`, `internal/service/chat_errors.go`, `internal/metrics/testdata/metrics.golden`

**Done when.**
- Opening the circuit for `azure-foundry#<res>|my-gpt` produces a transition sample labelled `{provider=azure-foundry, model=deployment, resource=<8-char hash>}`, and the log line names the same values.
- `pad_ai_circuit_breaker_open{provider}` is 1 while the circuit is open and returns to 0 on close. A test asserts that the model label comes from the catalogue or is `deployment`/`uncatalogued`.
- `errors.Is(err, ErrCircuitOpen)` still holds, and the chat error text includes the model and the remaining cooldown.

### R-observability-8

**ai_request_duration_seconds cannot observe stream latency: buckets stop at 10 s and there is no time-to-first-token** · tier 2

**Problem.** The histogram uses `prometheus.DefBuckets` (5 ms to 10 s), but it is observed only after the whole Stream lifetime, retries and backoff included (up to 30 s per wait). Streams on reasoning models legitimately run for minutes, up to the 10-minute wall clock. Every stream or turn longer than 10 s lands in +Inf, where one cannot be told from another. The metric baki exposes therefore cannot give p50/p95 stream latency or time-to-first-token, the first SLOs a reliability roadmap needs. Chat (`audited.go:164-166`) and Embed (`audited.go:637-639`) feed the same histogram under the same provider-only label, so fast embeds and long streams are mixed in one distribution.

**Evidence.**
- `internal/ai/metrics.go:25` — `aiRequestDuration` uses `Buckets: prometheus.DefBuckets`, and its Help says "the lifetime of a Stream" (25-32)
- `internal/ai/audited.go:198` — `start := time.Now()` runs before `inner.Stream`, and `observeAIRequest` runs after it returns (259), so after retry, backoff and the whole SSE body
- `internal/ai/retry.go:21` — `retryMaxDelay` is 30 s per backoff wait
- `internal/service/chat.go:36` — `maxChatStreamDuration` is 10 min

**Change.** Add a new metric, for example `ai_stream_duration_seconds` or `ai_request_duration_seconds_v2`, instead of re-bucketing `ai_request_duration_seconds` in place. Changing the buckets of an existing series breaks range quantiles across the change (`internal/metrics/metrics_golden_test.go:201-203`). Give the new metric an explicit bucket list from 0.1 s to 600 s (0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600) and a kind label (chat, stream, embed). Add a separate `ai_stream_first_token_seconds` histogram, observed in the audited wrapper on the first chunk with Text, Reasoning or ToolCalls (`audited.go:228-238` already computes `sawContent`). `metrics.golden` does not record buckets, so pin the new ones in `TestMetrics_HistogramBuckets` (`metrics_golden_test.go:208`) and refresh the metrics golden.

**Touches.** `internal/ai/metrics.go`, `internal/ai/metrics_test.go`, `internal/ai/audited.go`, `internal/metrics/testdata/metrics.golden`, `internal/metrics/metrics_golden_test.go`

**Done when.**
- A 60 s observation lands in a finite bucket, and after it the +Inf bucket count equals the 600 s bucket count.
- `ai_stream_first_token_seconds` is observed exactly once per Stream that delivers content.
- `TestMetricsGolden` and `TestMetrics_HistogramBuckets` are updated and pass.

### R-observability-9

**TracedProvider spans ignore error chunks, served model, cache tokens and span status, and have no test** · tier 2

**Problem.** `TracedProvider` sets `ai.model` to the requested alias and records `ai.tokens_in`/`ai.tokens_out`, plus `ai.finish_reason` on Chat only. Stream's chunk callback copies tokens and nothing else. It never records `c.Error`, `ServedModel`, the cache slices, `ToolCalls` or the finish reason, and it never calls `SetStatus`. The SSE parsers report an upstream failure as `Chunk{Error}` and then return nil, which is exactly the case the breaker handles. What the span shows depends on when that error arrives, because in the only production chain `RetryingProvider` sits inside `TracedProvider` (`factory.go:265-267`).

- **Before any content:** `RetryingProvider` holds back the error chunk and returns it as an error, with its usage carried in `heldUsageError` (`retry.go:256-275`). Traced's `RecordError` fires, but the span gets no `codes.Error` status and loses the tokens. The held-back chunk never reaches the traced callback, and Traced does not read `usageOf(err)` (`retry.go:219-225`).
- **After content:** this is typical of `content_filter` finishes and Claude refusals (`stream.go:442-446`). The error chunk passes through (`retry.go:257`, `:276`) and the inner Stream returns nil. The stream traces as a success-shaped span with the deployment name and the last token values seen.

CountTokens and Models are not spanned, and the audited wrapper refuses over-budget calls outside the traced layer. There is no `tracing_test.go`, so a regression in span attributes or error recording goes unnoticed.

**Evidence.**
- `internal/ai/tracing.go:61` — Stream span attributes are `ai.provider` and `ai.model` (the request alias). The chunk callback (70-78) copies tokens only, and `RecordError` runs only for the returned err (80-82). There is no `SetStatus` and nothing from `c.Error`, `ServedModel`, `Cache*` or `FinishReason`.
- `internal/ai/tracing.go:25` — Chat records only tokens and `finish_reason` (38-42), never `ServedModel` or cache tokens
- `internal/ai/circuitbreaker.go:228` — "the SSE parsers surface an upstream error as Chunk{Error} and then return nil"
- `internal/ai/stream.go:472` — a Claude mid-stream error is delivered as an error chunk, then `return true`, so the stream returns nil
- `internal/ai/audited.go:677` — CountTokens (677-688) and Models (689-707) are not traced. `admit` (195) runs outside the traced layer, so a budget refusal is never spanned.
- `internal/ai/tracing.go:12` — the `TracedProvider` type; Glob `internal/ai/tracing*_test.go` finds no file

**Change.** Add these span attributes: `ai.served_model`, `ai.cache_read_tokens`/`ai.cache_write_tokens`, `ai.finish_reason` and `ai.error_class` (a closed set drawn from the sentinels). Read `usageOf`/`cacheUsageOf` from a returned error, so a held-back chunk's tokens still reach the span. Call `RecordError` and set `codes.Error` status on a returned error or an error chunk. Add spans for CountTokens and Models.

A budget refusal happens in `audited.admit`, which runs before `TracedProvider` (`audited.go:195`; `factory.go:267` wraps Traced inside audited). An `ai.budget_refused` attribute therefore cannot go on the provider span. Record the refusal as a span event in the audited wrapper, or rely on the existing `pad_ai_budget_refusals_total` counter (`metrics.go:50`).

Add `tracing_test.go` using the sdktrace in-memory exporter. It asserts attributes, status and error events for Chat, Embed and both stream failure shapes, on `TracedProvider` alone and through the factory chain. Seeing attempts under retries is [R-observability-10](observability.md#r-observability-10).

**Touches.** `internal/ai/tracing.go`, `internal/ai/tracing_test.go`, `internal/ai/audited.go`

**Done when.**
- Tested on `TracedProvider` alone, a Stream whose provider emits `Chunk{Error: ErrContentFiltered, TokensIn: 10}` and then returns nil ends a span with status Error, one exception event, `ai.error_class=content_filtered` and `ai.tokens_in=10`.
- Through the factory chain, the same error chunk before any content ends the span with status Error, `ai.error_class=content_filtered` and `ai.tokens_in` taken from `heldUsageError`.
- A Stream whose Done carries ServedModel `gpt-4o-2024-11-20` sets `ai.served_model` on the span, and a successful stream's `ai.tokens_in`/`ai.tokens_out` equal the Done chunk's counts.
- `tracing_test.go` exists and covers Chat, Embed and Stream with the in-memory exporter.

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

### R-observability-4

**ai_request_errors_total counts user stops and watchdog cancellations as provider errors** · tier 3

**Problem.** The audited wrapper increments `ai_request_errors_total{provider}` on any non-nil error from Stream, Chat or Embed. Several things cancel the stream ctx: a user pressing Stop, the idle watchdog, the subscriber-gone watchdog, Shutdown and the 10-minute cap. The SSE readers return the scanner's `context.Canceled` wrapped as "reading … SSE stream", so each of these counts as a provider error. The per-turn RAG query embed adds to it: when its 800 ms deadline fires, Embed counts the error too. The retry package already tells ctx errors apart (`isTransportError`), but the metric does not, and there is no error-class label. The only provider-error metric is therefore polluted by client behaviour. No alert on it exists today: only `metrics.golden:3` references it, and nothing in `operations.md` does. A future alert on its rate would fire during a busy afternoon of users stopping long answers, and a real provider degradation would hide under that noise.

**Evidence.**
- `internal/ai/audited.go:260` — `if err != nil || sawChunkErr { recordAIError }` with no `context.Canceled` or `DeadlineExceeded` exclusion; Chat (167-168) and Embed (640-641) do the same
- `internal/service/chat_context.go:33` — `ragGuidelinesDeadline` = 800 ms bounds the per-turn query embed, which fails through Embed's error path
- `internal/ai/retry.go:27` — `isTransportError` excludes `context.Canceled` and `DeadlineExceeded`, so the package knows the distinction
- `internal/ai/stream.go:514` — `scanner.Err()` (the cancelled body read) is wrapped and returned (519)
- `internal/service/chat_resume.go:232` — the idle watchdog cancels through `ctl.cancelWithReason`; subscriber-gone does the same at 250; `chat_stream_control.go:272` gives both an empty code, and `chat_errors.go:231-232` reports that as `STREAM_STOPPED`
- `internal/ai/metrics.go:33` — Help text "AI provider requests that ended in an error, by provider"; the only label is `provider`

**Change.** In `auditedProvider.Chat`, `Stream` and `Embed`, skip `recordAIError` when `errors.Is(err, context.Canceled)`, or when the ctx is done and the error is not a provider sentinel. Add a closed-set `class` label derived from the existing sentinels: `rate_limited`, `provider_down`, `auth`, `permission`, `context_limit`, `content_filtered`, `output_limit`, `transport`, `truncated`, `other`. Add a separate `pad_ai_requests_cancelled_total{provider, reason}` with reason one of `client`, `idle`, `subscriber_gone`, `shutdown` or `deadline`. It is fed from the stream's `cancelCode`. Idle, subscriber-gone and user stop all cancel with an empty code today, so those call sites must set distinct codes first. Refresh the metrics golden, and add the corrected family to the `operations.md` alert list.

**Touches.** `internal/ai/audited.go`, `internal/ai/metrics.go`, `internal/ai/metrics_test.go`, `internal/service/chat_stream_control.go`, `internal/metrics/testdata/metrics.golden`, `docs/architecture/operations.md`

**Done when.**
- A stream cancelled by ctx (user stop) leaves `ai_request_errors_total` unchanged and increments `pad_ai_requests_cancelled_total{reason="client"}`.
- A stub 429 after retries increments `ai_request_errors_total{class="rate_limited"}`, and a 401 increments `class="auth"`.
- The metrics golden and the cardinality test pass with the closed label sets.

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

`chat_stream_active` is a bare count, and no endpoint lists active streams. `streamCtl` holds owner, org, flow, conversation key, in-flight cost and `lastActivity` (`chat_stream_control.go:100-131`). It does not hold provider, requested model, start time or iteration count, so a listing needs those fields added to `streamCtl`. The cancel route refuses anyone but the owner. Until the 10-minute wall-clock cap (`chat.go:36`, `704`) or the 90 s idle watchdog (`chat_resume.go:50`, `230`) fires, only its owner can stop a runaway or stuck stream. Breaker state exists only as a transitions counter and a log line; the gauge is [R-observability-7](observability.md#r-observability-7).

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

### R-observability-12

**No soft threshold, notification or velocity limit: the only cost control is a hard daily refusal that every other user then shares** · tier 3

**Problem.** `check()` returns an error or nil. Nothing publishes an event when a scope or org crosses 50, 80 or 100 % of its cap. The mid-loop stop reaches the user as a budget error (`chat.go:1113`) and goes to error reporting (`chat.go:1111`), but raises no operator or admin event. The HTTP limiters count requests per IP or per user, and the per-scope concurrency cap is 3; none of them is cost-aware. `PerUserDailyBudget` caps one member's share of an org's day when it is set (`ai_budget.go:228-238`). When it is not set, one caller such as a harness can burn an org's whole day in minutes, and every member is then refused for the rest of the day with no warning. Operators learn about it afterwards from `pad_ai_budget_refusals_total`. The notify package already has a channel-kind registry that org alerts could use.

**Evidence.**
- `internal/service/ai_budget.go:199` — `check()` returns an error or nil and publishes no event (199-241)
- `internal/service/ai_budget.go:228` — the per-user sub-limit applies only when `PerUserDailyBudget > 0` (228-238); no file under `frontend/src` references it
- `internal/service/chat.go:1108` — mid-loop stop: `errreport.CaptureError` (1111) and the user-facing failure (1113); no event
- `internal/ai/metrics.go:50` — the refusals counter is the only signal (47-56)
- `internal/notify/kinds.go:164` — the channel-kind registry (ADR 0021) exists for org notifications; `Kind()` at 201
- `internal/config/config.go:422` — chat 3 rps, burst 10 per IP; per-user 10 rps, burst 20 (422-437). These count requests, not cost; `maxConcurrentStreamsPerScope = 3` at `chat.go:39`

**Change.** `PerUserDailyBudget` is the existing mitigation for one user draining the org; name it and make it reachable. Do the cheaper parts first:
- Expose `PerUserDailyBudget` in the settings UI.
- Put `usedRatio` on the done payload, so clients can warn before the last turn.

As follow-ups:
- Publish `events.BudgetThreshold{Scope, OrgID, Ratio}` from `check()` when a crossing is first observed, deduplicated per day in Postgres or Redis. Deliver it to org admins through a notify kind (ADR 0021, with its golden) and the in-app notifier.
- Add an optional hourly velocity cap (`AI.HourlyBudget`) with the same check.

Tier 3.

**Touches.** `internal/service/ai_budget.go`, `internal/events/`, `internal/notify/`, `internal/service/chat.go`, `frontend/src/components/chat/`

**Done when.**
- The settings UI edits `PerUserDailyBudget`, and the done payload carries `usedRatio`.
- Crossing 80 % of an org cap publishes exactly one `BudgetThreshold` event per org per day across replicas.
- The org admins' configured channel receives the payload, and the notify golden is updated.
- With `HourlyBudget` at $1 and $1 spent in the last hour, the next call is refused with a distinct code and `Retry-After`.

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

### R-observability-14

**Parameter repairs, route fallbacks, stream refusals, replay rebuilds and Entra token invalidations are debug/warn logs only; no counters exist** · tier 3

**Problem.** The provider layer heals itself in several ways:
- dropping a rejected parameter
- rerouting to the Responses API
- answering non-streaming for a model that refuses streaming
- splitting an embedding batch
- rebuilding a replayed history
- invalidating an Entra token

Each of these is visible only as a log line, most at Debug. Two cases cannot be seen on a dashboard: a deployment that pays a rejection round trip on every turn because its memo keeps being evicted or forgotten, and a resource whose tokens are refused once a minute. `pad_ai_retry_attempts_total` covers only the retry wrapper's two closed reasons.

**Evidence.**
- `internal/ai/openai_compat.go:275` — tools-with-reasoning repair: `logger.Debug`; value rejection Warn at 306; generic repair Debug at 324; stream refusal Debug at 754; dimensions refusal at 1080; batch split at 1089
- `internal/ai/responses.go:533` — replay rebuild Debug; parameter repair at 551; route refusal at 615 and 709
- `internal/ai/azure_foundry.go:192` — token invalidation: `logger.Warn` only
- `internal/ai/metrics.go:10` — registered AI metrics (104-107): breaker transitions, tokens, duration, errors, usage dropped, budget refusals, pricing fallback, cost, retry attempts and hosted tool steps; none covers repairs or fallbacks

**Change.** Add counters with closed label sets:
- `pad_ai_param_repairs_total{provider,param}`, with `param` taken from `repairableParams` plus `reasoning_effort=none`
- `pad_ai_route_fallbacks_total{provider,kind}`, with kind one of `stream_refused`, `responses_reroute`, `responses_refused`, `replay_rebuild`, `embed_split` or `dimensions_dropped`
- `pad_ai_entra_token_events_total{kind}`, with kind one of `refreshed`, `refresh_failed`, `invalidated` or `rejected`
- `pad_ai_memo_evictions_total{memo}`

Refresh the metrics golden.

**Touches.** `internal/ai/metrics.go`, `internal/ai/openai_compat.go`, `internal/ai/responses.go`, `internal/ai/azure_foundry.go`, `internal/ai/azure_entra.go`, `internal/metrics/testdata/metrics.golden`

**Done when.**
- A request repaired once increments `pad_ai_param_repairs_total{param="temperature"}` by one, and a request with the memo pre-applied does not.
- `TestMetricsGolden` is refreshed, and a test asserts that every new label value belongs to a closed set.

### R-observability-16

**Copilot listing errors embed up to 10 MiB of response body in the error string, which is also cached for 15 s** · tier 3

**Problem.** `fetchModels` formats the whole body it read, up to 10 MiB, into the error on a non-200 status and again on a decode failure. The `modelListCache` keeps that error for `copilotModelsErrCacheTTL`. Two things follow. Log lines carry up to 10 MiB of upstream HTML, from `providers.go:307` and `:734`, on every listing within the 15 s error window. And up to 512 cached error strings of up to about 10 MiB each are held for 15 s (`copilot.go:51`, `55`; `model_list_cache.go:56-64`). Other paths bound service text to one line of 500 bytes with `oneLine`.

**Evidence.**
- `internal/ai/copilot.go:148` — the body is read with `io.LimitReader(10<<20)` and formatted into the error (150)
- `internal/ai/copilot.go:160` — a decode failure also formats the body (161)
- `internal/ai/copilot.go:51` — `copilotModelsErrCacheTTL` = 15 s caches the error (used at 120); the cache holds up to 512 entries (55)
- `internal/ai/model_list_cache.go:56` — a failed fetch is stored with its error for `errTTL` (56-64)
- `internal/service/providers.go:307` — the listing error is logged with the full error text; `:734` likewise
- `internal/ai/azure_entra.go:194` — `oneLine`: the bounding helper other paths use

**Change.** Keep the status and `oneLine(body, 500)` on the error, and log the request id header when one is present.

**Touches.** `internal/ai/copilot.go`, `internal/ai/copilot_provider_test.go`

**Done when.**
- A 1 MiB HTML 502 from the models URL yields an error under 1 KiB that contains "status 502".

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
