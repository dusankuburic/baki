# AI roadmap: Budgets and cost

Items of category `budget` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-budget-1

**Retry wrapper drops a failed attempt's held usage when a later attempt succeeds (Stream, Chat and Embed)** · tier 1

**Problem.** `RetryingProvider.Stream` withholds an error chunk that arrives before content so the attempt can be retried. It keeps that chunk's `TokensIn`, `TokensOut`, cache slice and `ServedModel` in locals that are re-declared on every attempt. The usage is re-attached as `heldUsageError` only when the attempt's final error is non-nil. Take a retryable held error, such as a Claude `overloaded_error`, `api_error` or `timeout_error` after `message_start`, which carries `input_tokens` and cache counts. If a successful attempt follows it, the first attempt's tokens are lost. The audited wrapper does see the failed attempt's `message_start` chunk, because only Error chunks are withheld, but it overwrites those counts with the next attempt's instead of summing them, and the successful second attempt returns nil. The `Chat` and `Embed` loops have no held-usage carry at all: they keep `lastErr` and retry without reading `usageOf(err)`.

`heldUsageError` was built on the premise that the provider bills every attempt. Baki records only the last attempt, so the daily budget is under-counted in exactly the cases `heldUsageError` was built for. The under-count is real for direct Claude streams that get a pre-content `overloaded_error`, `api_error` or `timeout_error` after `message_start` and then succeed on retry. Two other cases only affect token counts on an unpriced, budget-exempt provider (Azure AI Foundry): an agent run that fails `server_error` or `no_capacity` after consuming tokens, and the `Chat`-loop carry. The `Embed` part has no effect, since Embed errors never carry usage.

**Evidence.**
- `internal/ai/retry.go:251` — `delivered`, `heldErr`, `heldTokensIn`/`heldTokensOut`, `heldCacheRead`/`heldCacheWrite` and `heldServedModel` are declared inside the attempt loop (251-255) and reset on each attempt. The pre-content error chunk is swallowed at 257-261 and never forwarded.
- `internal/ai/retry.go:270` — `heldUsageError` is built only when `err != nil` (270-275). A retryable error loops at 276-279, and the held usage is dropped.
- `internal/ai/retry.go:149` — the `Chat` loop (149-167) keeps only `lastErr` and never reads `usageOf(err)` before retrying. `Embed` (184-200) does the same.
- `internal/ai/retry.go:202` — `heldUsageError` exists so "the audited provider can still bill a run whose chunks it never saw" (202-203). This is the premise that every attempt is billed.
- `internal/ai/stream.go:363` — `message_start` emits a usage-only chunk (`TokensIn` and cache counts), which the retry wrapper forwards.
- `internal/ai/stream.go:472` — Claude's error event emits `Chunk{Error, TokensIn, CacheReadTokens, CacheWriteTokens}`. `classifyClaudeStreamError` (63-66) maps `overloaded_error`, `api_error` and `timeout_error` to `ErrProviderDown`, which is retryable.
- `internal/ai/audited.go:212` — `wrappedOnChunk` overwrites `tokensIn`, `tokensOut` and the cache slice with each chunk's values instead of summing them (212-220).
- `internal/ai/audited.go:267` — `Stream` reads `usageOf`/`cacheUsageOf` from the returned error only (267-280). `Chat` bills usage only from the error finally returned (173-178).
- `internal/ai/responses.go:648` — a failed agent run returns `heldUsageError` with tokens. `classifyResponsesError` (1137-1138) makes `server_error`/`no_capacity` `ErrProviderDown`.
- `internal/ai/spec_azure_foundry.go:34` — Azure AI Foundry is `PricingUnpriced`, and `internal/ai/audited.go:133` exempts alias providers from the budget.

**Change.** In `RetryingProvider.Stream`, accumulate held usage across attempts by summing `tokensIn`/`tokensOut` and the cache slices of every discarded attempt. Then surface it in one of two ways:

- On success, as a usage-only chunk that the audited wrapper folds into the recorded row.
- Through an optional sink the factory sets for each discarded attempt's usage, for example `rp.onDiscardedUsage = ap.recordRetriedUsage`. The sink records one row per failed attempt on the same pricing path as a failed call.

Either way, the fix must stop the next attempt's `message_start` from overwriting the count, for example by summing the discarded attempts' usage separately. In `Chat`, read `usageOf(err)` before each retry and route it to the same sink. The `Embed` loop needs no change, because its errors carry no usage. Keep the final-attempt semantics and the keepalive (field-less `Chunk`) semantics intact. Add retry tests for both paths and an audited end-to-end test that asserts the totals.

**Touches.** `internal/ai/retry.go`, `internal/ai/audited.go`, `internal/ai/provider.go`, `internal/ai/factory.go`, `internal/ai/retry_test.go`, `internal/ai/audited_test.go`, `internal/ai/audited_chat_error_usage_test.go`

**Done when.**
- Use a Stream stub whose first attempt emits `Chunk{Error: ErrProviderDown, TokensIn: 1000, CacheReadTokens: 400}` before content and whose second attempt ends `Done(500,100)`. It records `PromptTokens` totalling 1500 (one row or two), with the retried attempt's cache slice priced at the cached rate.
- A Chat stub that returns `&heldUsageError{err: ErrProviderDown, tokensIn: 300}` and then a success records the 300 input tokens.
- Two cases still produce exactly the usage they produce today, with no double count of the last attempt: a failed-then-failed sequence, and a held error without usage followed by a success.

### R-budget-3

**The no-usage estimate fallback omits tool schemas and tool-call arguments and is skipped when the provider reports only one side** · tier 1

**Problem.** A provider can report no usage in two ways. Compat servers behind the `OPENAI_API_URL`, GLM or xAI overrides that ignore `stream_options` end with `Done(0,0)`. Any stream cancelled, truncated or errored before `Done` also has none. In both cases the audited wrapper substitutes estimates, but the estimate is incomplete on both sides.

- **Input.** `estimateRequestTokens` sums the system prompt, message contents, tool-call arguments and replay records, but never reads `req.Tools`. Yet the context-window check counts the schemas, and `runToolLoop` attaches all tool definitions to every iteration. Every tool-loop iteration on such a server is therefore short by the size of all tool schemas. So is every cut tool turn on a provider that reports usage only at the end (OpenAI-compatible with `include_usage`, Gemini). `requestEstimateHash` omits the schemas too, so the shortfall is memoised. Claude reports input tokens at `message_start`, so a cut Claude stream never runs the input estimate.
- **Output.** `estOut` accumulates only `Text` and `Reasoning` deltas. A turn ending in tool calls, which is the case that triggers the fallback via `len(ToolCalls) > 0`, records `tokensOut` 0, even for a multi-kilobyte `apply_fixes` payload. This applies to every provider's estimate path.
- **One side reported.** The `Done` path requires both counts to be zero. A server reporting `prompt_tokens` with `completion_tokens` 0 therefore gets no output estimate, while the no-`Done` path estimates each side independently.

**Evidence.**
- `internal/ai/audited.go:397` — `estimateRequestTokens` sums `SystemPrompt`, `m.Content`, `tc.Input` and `ProviderItems` only, and never reads `req.Tools` (397-416). `requestEstimateHash` fingerprints the same fields and omits `Tools` (375-393).
- `internal/ai/audited.go:248` — the `Done` fallback runs only if `tokensIn == 0 && tokensOut == 0 && (sawContent || len(c.ToolCalls) > 0)`. It sets `tokensOut = estOut.total()`, which only accumulates `c.Text` (228-231) and `c.Reasoning` (234-237); tool calls are never estimated.
- `internal/ai/audited.go:285` — the `!sawDone` path estimates `tokensOut` and `tokensIn` independently (286-291), which is inconsistent with the `Done` path.
- `internal/service/chat_context.go:432` — `promptTokenCeiling` counts `len(td.InputSchema)+8` per tool (432-434), and the drop loop's fixed component adds each schema's estimate (506-508).
- `internal/service/chat.go:1167` — `runToolLoop` sets `base.Tools = ai.ToolDefinitionsFor(tctx.Allow)` for every iteration.
- `internal/ai/openai.go:12` — the `OPENAI_API_URL` override keeps the openai provider's paid catalogue rates for compat servers that end with `Done(0,0)`.
- `internal/ai/stream.go:359` — Claude reports input tokens at `message_start` (359-364).

**Change.** Add the tool definitions to `estimateRequestTokens` and `requestEstimateHash`: name, description, `InputSchema` and the per-tool framing the window check assumes. Use the same per-tool cost as the `chat_context` drop loop so the two estimates agree; consider one shared request estimate used by both packages. On `Done`, add `EstimateTokens(tc.Input)` (plus the name) for each tool call to the output estimate. When one side is zero and the other is reported, estimate the zero side independently. Pin all three cases in `audited_stream_estimate_test` and `estimate_memo_test`.

**Touches.** `internal/ai/audited.go`, `internal/ai/estimate_memo_test.go`, `internal/ai/audited_stream_estimate_test.go`, `internal/ai/audited_test.go`, `internal/service/chat_context.go`

**Done when.**
- `estimateRequestTokens` for a request with the 8 `ToolDefinitions` exceeds the same request without `Tools` by at least the sum of `EstimateTokens(InputSchema)` per tool, and `requestEstimateHash` differs between the two.
- A stream ending `Done(0,0)` with one 2 KB tool call records `tokensOut` ≈ `EstimateTokens(args)`, not 0, and its `PromptTokens` include the schema estimate.
- A stream ending `Done(in=500,out=0)` with 1 KB of text records an estimated `tokensOut`.

### R-budget-5

**In-flight reservation for an unset Max tokens is half of Claude's wire default** · tier 1

**Problem.** `maxOutputTokens` returns 4096 when the request leaves `MaxTokens` unset. On a large window, `turnGenerationParams` leaves the request at 0 for non-reasoning models: claude-sonnet-4-5, claude-haiku-4-5 and opus before 4.7. For these, `claudeRemovesSampling` is false, so the reasoning floor is 0. `ClaudeProvider.buildBody` then sends `max_tokens` 8192 by default, while `prepareTurn` reserved `inFlightCost(..., 4096)`, and the loop reserve uses the same 4096 basis. The documented ledger bound ("reservations total less than the budget plus one reservation") is therefore violated by up to 4096 output tokens per concurrent Claude stream that leaves Max tokens unset, which is the common default. The OpenAI-compatible and Gemini routes default to 4096, so only the Claude route diverges.

**Evidence.**
- `internal/service/chat.go:104` — `maxOutputTokens` returns `req.MaxTokens` or 4096 (104-109).
- `internal/service/chat.go:141` — `requested` stays 0 unless `m.maxOutput > 0` or the window is small (141-149). `reasoningFloorTokens` raises only models with a non-zero floor (151-153).
- `internal/service/chat.go:547` — `prepareTurn` reserves `maxOutputTokens(Request{MaxTokens: reasoningFloorTokens(req.MaxTokens, ...)})`, which is 4096 for a non-reasoning Claude model (547-549).
- `internal/service/chat.go:1076` — the tool loop's `reserve` is `inFlightCost(price, 0, maxOutputTokens(base))`, on the same basis.
- `internal/ai/claude.go:423` — `buildBody` sets `MaxTokens: orDefault(req.MaxTokens, 8192)`.
- `internal/ai/claude.go:374` — `claudeRemovesSampling` is false for sonnet/haiku 4.x and opus before 4.7 (374-397), so `ReasoningOutputFloor` (`reasoning_floor.go:42-47`) returns 0 for them.
- `internal/ai/openai_compat.go:138` — `orDefault(req.MaxTokens, 4096)`. Gemini does the same at `gemini.go:329` and `gemini.go:439`, so only the Claude route diverges.

**Change.** Make the chat service decide the wire max tokens. When the resolved `MaxTokens` is 0, set `aiReq.MaxTokens` explicitly to the provider's default before reserving, so the reservation and the request share one number. The default comes from a `DefaultMaxOutputTokens()` on the provider or spec: 8192 for Claude, 4096 for the others. Alternatively, lower Claude's default to 4096 via the spec. Pin the result with `chat_context_window_test` and `chat_turn_lifecycle_test`.

**Touches.** `internal/service/chat.go`, `internal/ai/claude.go`, `internal/ai/provider.go`, `internal/ai/spec.go`, `internal/service/chat_context_window_test.go`, `internal/service/chat_turn_lifecycle_test.go`

**Done when.**
- For a Claude model with `ReasoningOutputFloor` 0 and no user Max tokens, `ctl.inflight()` after `prepareTurn` equals `inFlightCost(price, 0, N)`, where N is the `max_tokens` the Claude body actually serialises.
- A table test over the catalogue asserts that the reservation tokens are at least the wire `max_tokens` for every provider family.

### R-budget-6

**The in-flight reservation and ledger are priced at the provider DEFAULT model's rate, not the turn's model; ReservationPricer is unused** · tier 1

**Problem.** `prepareTurn` reserves one max response at `inFlightPricing(provider)`, which ignores its model argument and returns `p.PricePerMillionTokens()`. Every provider implements that as `specDefaultPricing`, the catalogue row of the spec's `DefaultModel`. The reservation, the shared Redis ledger entry that other replicas read, and the tool loop's own in-flight cost are all computed at that rate. The loop's cost is `recheckBudget`'s `own = inFlightCost(st.price, ...)`.

| Turn model (rate) | Reserved at | Effect |
|---|---|---|
| claude-fable-5-1 (10/50) | claude-sonnet-4-6 (3/15) | 3.3x under |
| grok-3 (3/15) | grok-3-mini (0.3/0.5) | 10-30x under |
| claude-haiku-4-5 (1/5) | claude-sonnet-4-6 (3/15) | 3x over |

Inside the tool loop, the ledger's `own` and the done payload's rates price the same tokens differently: the done payload goes through `addTurnUsage`, which uses `UsagePricing` per served model. A model pricier than the default under-covers and widens the cross-replica overshoot the ledger was built to bound. A model cheaper than the default over-reserves and refuses starts near the cap. The audited wrapper's `ReservationPricing` returns the same provider-wide rate, although `pricingFor` can resolve per-model listing prices, and `served_model.go` records that the chat service does not consult it.

**Evidence.**
- `internal/service/chat.go:55` — `inFlightPricing` returns `Pricing{}` for an alias provider and `p.PricePerMillionTokens()` otherwise. The model argument is ignored (55-59).
- `internal/ai/claude.go:81` — `PricePerMillionTokens` is `specDefaultPricing(claudeSpec)`. The same holds at `openai.go:51`, `xai.go:32`, `glm.go:49` and `gemini.go:61`. `specDefaultPricing` (`spec.go:414-421`) returns the `DefaultModel`'s catalogue row.
- `internal/ai/catalog.go:15` — claude-fable-5-1 is 10/50 and claude-sonnet-4-6 is 3/15 (:30; the claude `DefaultModel` per `spec_claude.go:15`). grok-3 is 3/15 (:44) and grok-3-mini is 0.3/0.5 (:45; the xai `DefaultModel` per `spec_xai.go:14`).
- `internal/service/chat.go:549` — the reservation is `inFlightCost(inFlightPricing(...), 0, reserveTokens)`. `recheckBudget` (1102) prices `own` at `st.price`, while `addTurnUsage` (1132-1146) prices the same tokens via `UsagePricing` per served model.
- `internal/ai/audited.go:582` — `ReservationPricing` returns `Pricing{}` when unpriced and `inner.PricePerMillionTokens()` otherwise (582-587), although `pricingFor` (605-616) resolves per-model listing prices.
- `internal/ai/served_model.go:245` — `ReservationPricer`: "The chat service does not consult it today" (245-252). `docs/architecture/ai-providers.md:20` notes "A start stampede can refuse all of its members".

**Change.** Resolve the reservation rate per model. Make `ReservationPricing` use `UsagePricing`/`pricingFor`: the catalogue or listing row for the requested model, or the served model when known. Fall back to the provider default only when nothing prices the model. Have `prepareTurn` and `recheckBudget` call the `ai.ReservationPricer` when the provider implements it, and use the same resolved rate for the loop's `own`. Ignore the cached-token multipliers when reserving, which errs on the safe side.

Update the test that pins the current behaviour. `chat_turn_lifecycle_test.go:228-235` asserts that "A non-alias provider never consults the interface" and that the reservation uses the provider-wide rate. Correct the comment at `served_model.go:246-249` ("which answers the same") as well. The usage rows themselves are already priced per model (`audited.go:513-517`); only the in-flight and ledger component is mispriced. Add a `chat_turn_lifecycle` test with a non-default model.

**Touches.** `internal/service/chat.go`, `internal/ai/audited.go`, `internal/ai/served_model.go`, `internal/service/chat_turn_lifecycle_test.go`, `docs/architecture/ai-providers.md`

**Done when.**
- A claude-fable-5-1 turn with the 16 384 reasoning floor reserves 16384 × 50/1e6 = $0.82, not $0.25. A turn on a model priced at 5x the default publishes a ledger entry 5x larger.
- After iteration k, the loop's ledger entry equals the sum of the per-iteration priced usage plus one reservation at the model's rate.
- The D-21 test still holds: the reservation is published before the start check.

### R-budget-7

**The reservation covers only the output: the prompt is never reserved, a plain stream never refreshes its entry, and a stream's own reservation is excluded from its own admission** · tier 1

**Problem.** `prepareTurn` reserves `inFlightCost(price, 0, reserveTokens)` with `tokensIn` 0 and then calls `enforceTurnBudget` with inflight 0, so the reservation covers one response and no prompt. The prompt is built afterwards (`buildStreamRequest`), and its size never reaches the ledger. A plain stream calls `setInflight` exactly once, so its entry stays output-only for the whole turn, and the audited hook's `Check` passes inflight 0. On the 1M-window Claude rows at $10/M, a 300k-token conversation costs $3.00 per turn uncached, and no check sees that cost before the usage row lands. A cold cache write raises it to $3.75; a cache hit on fable-5-1 lowers it to about $0.075.

**Evidence.**
- `internal/service/chat.go:549` — the reservation has `tokensIn` 0 (549), then `enforceTurnBudget(ctx, provider, scope, orgID, 0, ctl)` runs (551).
- `internal/service/ai_budget.go:199` — `check` computes total = usage + inflight + concurrent, where `others()` excludes self (212-220).
- `internal/service/chat.go:885` — `buildStreamRequest` (prompt assembly) runs after the reservation. Lines 894-895 say "The loops below refresh the reservation", but the plain path (937-990) never calls `setInflight` again.
- `internal/service/chat.go:629` — `buildStreamRequest` calls the non-memo `truncateForContextWindow`.
- `internal/service/chat_context.go:491` — on the fast paths, the only prompt size computed is the `promptTokenCeiling` byte ceiling, an upper bound. Lines 491-499 return without a token estimate, and 500-501 is the `nearWindow` closure.
- `internal/ai/audited.go:132` — the hook's `admit` calls `AIBudget.Check` (`ai_budget.go:167-169`) with inflight 0 and the same self exclusion, identical to the start check.
- `internal/ai/catalog.go:15` — 1 000 000-token windows at 10/50 (fable, mythos) make the prompt the dominant cost. `cacheWriteMultiplier` is 1.25 (186) and the fable-5-1 read rate is 0.025x (174-175), both applied in `audited.go:523-525`.
- `docs/architecture/ai-providers.md:20` — the documented bound: a stream "is admitted while the others' in-flight spend is under the budget". `internal/service/ai_budget_redis_test.go:62` pins it.

**Change.** After `buildStreamRequest`, set the ledger entry to the prompt's cost at the input rate plus the reservation. Re-check with inflight equal to that value; self stays excluded from others, so the stream's own cost is counted once. The prompt size at hand today is the `promptTokenCeiling` byte ceiling, an upper bound, because the fast paths compute no token estimate. In the tool loop, seed `own` with the prompt before iteration 1. For plain streams, refresh the entry at least once when the first chunk arrives; Claude reports usage on `message_start`.

Excluding a stream's own reservation from its own admission is the documented rule, not a defect, and it stays. The first criterion below changes the bound that rule gives. Rewrite the bound sentence in `ai-providers.md:20` as "prompt + one response per stream".

**Touches.** `internal/service/chat.go`, `internal/service/chat_context.go`, `internal/service/ai_budget.go`, `internal/service/chat_budget_concurrent_test.go`, `docs/architecture/ai-providers.md`

**Done when.**
- With a budget of $1.00, usage of $0.90 and a 100k-token prompt at $3/M (= $0.30), the turn is refused with `AI_BUDGET_EXCEEDED` before any provider call (a `FakeProvider` asserts zero calls).
- A plain stream's ledger entry, read by a concurrent start on another replica, includes its prompt cost.
- `ai-providers.md` states the overrun bound in terms of prompt + reservation.

### R-budget-8

**Budget memo pins the daily-usage read for the whole turn, so spend that lands during a long turn is invisible** · tier 1

**Problem.** `BudgetMemo` memoises the first `GetDailyUsage` per (scope, org) for the lifetime of the turn's context. That context is seeded at stream start and lives up to 10 minutes, through every tool-loop re-check and wrapper hook. The in-flight ledger only covers streams that are still running. The worker releases its entry in the deferred cleanup, while the usage row is written asynchronously and may be dropped under saturation. Every stream that finishes during a long turn therefore disappears from that turn's re-checks, because it is neither in the memo nor in the ledger. N concurrent long tool loops can thus overshoot the daily budget by their combined spend. That goes beyond the one-reservation bound in the ADR 0028 addendum (`docs/architecture/ai-providers.md:20`). The memo's own comment claims the ledger covers its staleness, and so do the comments on `enforceBudgetMemo` and `check`.

**Evidence.**
- `internal/ai/budget.go:62` — `BudgetMemo` memoises per request: "The checker's in-flight ledger covers the memo's staleness" (62-65). `Usage` returns the first read forever (85-101).
- `internal/service/chat.go:715` — `ctx = ai.WithBudgetScope(ai.WithBudgetMemo(withStreamSpend(ctx, ctl)), ...)` holds for the whole stream, and ctx is capped at `maxChatStreamDuration` (704).
- `internal/service/chat.go:1104` — `recheckBudget` uses `st.budgetUsage`, the turn's `BudgetMemo`, on every iteration (1104-1107).
- `internal/service/chat.go:828` — `s.aiBudget().release(ctl)` runs in the worker's deferred cleanup.
- `internal/ai/audited.go:431` — the usage row is written asynchronously after `acquireRecordSlot`/`goRecord` (431-438), with a 10 s save (501-503), and dropped when saturated (482-492).
- `internal/service/ai_budget.go:207` — `check` sums `memoUsage`, `inflight` and `ledger.others` (207-220).
- `internal/service/chat.go:68` — the `enforceBudgetMemo` comment repeats that the in-flight ledger covers the memo's staleness gap. `internal/service/ai_budget.go:196-197` says the same.
- `docs/architecture/ai-providers.md:20` — the one-reservation bound, stated in the ADR 0028 addendum.

**Change.** Choose one of:

- Give the memo a TTL: re-read after 30 s or after N re-checks.
- Invalidate the memo entry whenever any stream of the same scope/org releases its ledger entry on this replica.
- Keep a per-replica "recently finished" accumulator that bridges the gap from release to the row landing: the spend of streams released in the last 30 s, added to `others()`.

Update ADR 0028 §3/§4 and the bound in `ai-providers.md:20` to match. Also update the staleness comments at `budget.go:64`, `chat.go:68` and `ai_budget.go:196-197`.

**Touches.** `internal/ai/budget.go`, `internal/service/ai_budget.go`, `internal/service/chat.go`, `internal/service/chat_usage_memo_test.go`, `internal/service/chat_budget_concurrent_test.go`, `docs/adr/0028-ai-budget-hook-and-provider-policy.md`, `docs/architecture/ai-providers.md`

**Done when.**
- Test: turn A starts with usage $0 and a budget of $1. Stream B starts, spends $0.90 and finishes (its row is written) while A is between iterations. A's next re-check refuses with `AI_BUDGET_EXCEEDED`.
- A test with a fake usage reader pins the overshoot across K concurrent tool loops to at most K reservations plus the memo TTL window.

### R-budget-12

**Dropped or failed usage rows under-count the budget permanently and silently** · tier 1

**Problem.** Usage rows are lost in three places:

- `record()` takes a non-blocking slot (16 per wrapper) and drops the metric when all slots are busy.
- `goRecord` drops the metric when the process-wide runner (1024) refuses it.
- `save()` logs and forgets after a 10 s `SaveUsageMetric` failure.

There is no retry and no durable fallback. A Postgres brownout during a busy period therefore makes real spend vanish from `GetDailyUsage` for the rest of the day: the cap admits more than it should, and the cost dashboard disagrees with the provider invoice. The only signals are a WARN, `pad_usage_dropped_total`, and `pad_background_dropped_total{task="ai_usage_record"}` for runner refusals. A failed `SaveUsageMetric` has no metric at all.

**Evidence.**
- `internal/ai/audited.go:482` — `acquireRecordSlot` is non-blocking: when saturated it drops the row and calls `recordUsageDropped("saturated")` (482-492).
- `internal/ai/audited.go:450` — `goRecord`: when the runner refuses, it calls `releaseRecordSlot` and `recordUsageDropped("runner")` (450-460).
- `internal/ai/audited.go:501` — `save` uses a 10 s ctx, and a failure is only logged (501-509).
- `internal/ai/metrics.go:40` — `pad_usage_dropped_total{reason}` is the only usage counter (40-46). Its help text names `pad_background_dropped_total{task="ai_usage_record"}` for runner refusals (43). There is no counter for a failed save.
- `internal/ai/audited.go:428` — saturation is "in practice never hit" (428-430).

**Change.** Keep a process-local "unaccounted spend" accumulator per (scope, org), reset at the day boundary. Whenever a row is dropped or its save fails, `AIBudget` adds that spend to today's usage, which errs on the safe side. Retry a failed save once, with backoff, from a small bounded queue. Count failed saves in `pad_usage_save_failed_total` and document the alert. The save-failure path is the one that matters, since saturation is in practice never hit. Optionally, spool rows to a Redis stream drained by a lifecycle job while Postgres is unavailable.

**Touches.** `internal/ai/audited.go`, `internal/ai/metrics.go`, `internal/service/ai_budget.go`, `internal/metrics/testdata/metrics.golden`, `docs/architecture/operations.md`

**Done when.**
- Test: the recorder returns an error for a $3 row. With `DailyBudget` $5 and $2 already recorded, the next check on that scope is refused.
- Test: a row dropped by a saturated recorder is still counted by the budget on the same replica.
- `pad_usage_save_failed_total` appears in `metrics.golden`.

### R-budget-16

**Embedding usage is priced at the provider's default CHAT model rate (60-125x over) and bumps the pricing-fallback metric on every call** · tier 1

**Problem.** `auditedProvider.Embed` records usage under the embedding model name (`text-embedding-3-small`, `gemini-embedding-001`, `embedding-3`). `usageMetric` prices it through `pricingFor`, which only matches `m.ID` in the provider's `Models()` listing. The openai, gemini and glm listings are `catalogModels` of their chat catalogues, which have no embedding rows. The only `text-embedding` rows sit in the azure-foundry catalogue, which is unpriced. Every embed therefore falls back to `PricePerMillionTokens()`, the spec `DefaultModel`'s price:

| Provider | Charged at | Embedding model | Over by |
|---|---|---|---|
| OpenAI | gpt-4o, 2.5/M | `text-embedding-3-small`, ~0.02/M | about 125x |
| Gemini | gemini-2.5-pro, 1.25/M | `gemini-embedding-001`, ~0.15/M list | about 8x |
| GLM | glm-5.2, 1.4/M | `embedding-3` | roughly 20x |

On OpenAI, the default embedder, a full 2 500-chunk upload (~600k tokens) therefore records ~$1.50 instead of ~$0.01. Every per-turn RAG query embed is over-billed the same way. Each call logs "AI usage priced from provider default" and increments `pad_ai_pricing_fallback_total`. The token count is also an estimate, although the OpenAI-compatible embeddings responses (OpenAI, GLM) carry `usage.prompt_tokens`.

Today the budget harm is latent. In cloud the deployment `DailyBudget` is 0 until an admin sets one (`PUT /api/admin/settings/ai`), so the check returns nil, and the desktop has no usage store. What happens now is inflated AI spend in analytics, plus a Warn and a `pad_ai_pricing_fallback_total` increment on every embed. Over-refusal starts once a budget can be set.

**Evidence.**
- `internal/ai/audited.go:648` — `Embed` estimates `tokensIn` with `EstimateTokens` per text, names the model via `embedNamer` and calls `recordDetached` (648-666).
- `internal/ai/audited.go:513` — `usageMetric`: on a `pricingFor` miss it uses `p.inner.PricePerMillionTokens()` and calls `recordPricingFallback` and Warn on every call (513-519).
- `internal/ai/audited.go:605` — `pricingFor` finds a price only by `m.ID == modelID` in `p.Models(ctx)` (605-616). There is no catalogue lookup and no embedding table.
- `internal/ai/openai.go:25` — `embeddingModel` is `text-embedding-3-small`, and `Models()` (57-59) returns `catalogModels` of the openai catalogue. Likewise:
  - `internal/ai/gemini.go:52` uses `gemini-embedding-001`, with `Models()` returning `catalogModels` of the gemini catalogue (151-152).
  - `internal/ai/glm.go:22` uses `embedding-3`, with `Models()` returning `catalogModels` of the glm catalogue (59-60).
- `internal/ai/catalog.go:34` — the openai rows are gpt-4o, gpt-4o-mini and gpt-4-turbo only (34-38). The gemini (39-42) and glm (48-56) catalogues have no embedding rows. The only `text-embedding` rows (154-156) sit in the azure-foundry catalogue, unpriced.
- `internal/ai/spec.go:414` — `specDefaultPricing` returns the `DefaultModel`'s catalogue row:
  - gpt-4o at 2.5/10 (`catalog.go:35`, `spec_openai.go:14`)
  - gemini-2.5-pro at 1.25/10 (`catalog.go:40`, `spec_gemini.go:14`)
  - glm-5.2 at 1.4/4.4 (`catalog.go:52`, `spec_glm.go:14`)
- `internal/ai/gemini.go:131` — the `batchEmbedContents` decode reads only `embeddings` (131-138).
- `internal/service/ai_budget.go:203` — `check` returns nil when `DailyBudget <= 0` (203-205).
- `internal/di/services.go:243` — on the desktop there is no usage store, so spend reads $0 (243-244).

**Change.** Add embedding rows to the catalogue with output price 0: openai `text-embedding-3-small`, `text-embedding-3-large` and `text-embedding-ada-002`; gemini `gemini-embedding-001`; glm `embedding-3`. Azure stays unpriced. Make `pricingFor` consult `catalogEntryFor(providerID, modelID)` as a second source before the provider default. Parse `usage.prompt_tokens` in `decodeEmbeddings` (the OpenAI-compatible route, used by OpenAI and GLM) and prefer it over the estimate where the response carries it. Gemini's decode keeps the estimate. Keep the warning for genuinely unknown models only.

**Touches.** `internal/ai/catalog.go`, `internal/ai/audited.go`, `internal/ai/openai_compat.go`, `internal/ai/gemini.go`, `internal/ai/audited_pricing_test.go`, `internal/ai/embeddings_test.go`

**Done when.**
- An OpenAI embed of N tokens records `estimated_cost` = N × 0.02/1e6 (a test pins the row), and `pad_ai_pricing_fallback_total` does not increment.
- The "priced from provider default" warning never fires for a shipped embedding model.
- When the embeddings response carries `usage.prompt_tokens` (OpenAI, GLM), the row records that count, not the estimate.

### R-budget-4

**A plain (non-tool) stream is never budget re-checked mid-flight, contrary to the runbook and the ledger's own comment** · tier 3

**Problem.** A plain stream consults the budget only at its start: the start check in `prepareTurn`, and the audited wrapper's `admit` at the top of `Stream`. The per-iteration `recheckBudget` is a `toolLoopState` method called only from the two tool loops. Even there it runs between iterations, never inside one provider stream. The plain path calls `provider.Stream` once, and its chunk callback never consults the budget.

A long non-tool answer can be admitted just before the budget is exhausted, or just before Redis goes away on a multi-replica deployment with a daily budget. It then runs to completion with its reservation as the only bound. That can mean up to the 10 min wall clock, 16384+ output tokens on a reasoning model, and several such streams per user at once.

`operations.md`, `ai-providers.md` and the `redisLedger` comment all promise that "a running stream stops at its next re-check". That holds only for tool-loop turns, and only at iteration boundaries. Operators will therefore expect an outage or an exhausted budget to end streams that in fact keep spending.

**Evidence.**

- `internal/service/chat.go:937` — the plain path calls `ts.provider.Stream` once; the callback (937-990) touches, scrubs and emits, and never consults the budget
- `internal/service/chat.go:1101` — `recheckBudget` is a `toolLoopState` method; its only callers are `runToolLoop` (1190) and `runPromptToolLoop` (1401), each at the top of an iteration (1188-1192, 1399-1403)
- `internal/ai/audited.go:195` — `admit` runs once before `inner.Stream` (195-197); nothing inside `wrappedOnChunk` re-checks
- `docs/architecture/operations.md:108` — "a streaming answer stops at its next re-check"
- `docs/architecture/ai-providers.md:21` — "a running stream stops at its next re-check (\"couldn't verify today's AI budget\")"
- `internal/service/ai_budget_redis.go:24` — the `redisLedger` comment repeats "a running stream stops at its next re-check" (24-26)

**Change.** Add a mid-stream re-check to the plain path. Run it on the watchdog/heartbeat ticker that already runs per stream, or every N output chunks:

- Call `enforceTurnBudgetMemo` with `inflight = inFlightCost(price, 0, estimated output so far)`, excluding the stream itself, and refresh the memo per [R-budget-8](budget.md#r-budget-8).
- On refusal, cancel the provider call with the budget code through the `loopBudgetFailure` mapping.

The alternative is to keep plain streams unchecked and correct `operations.md`, `ai-providers.md` and the `redisLedger` comment to say that only tool-loop turns re-check. The re-check is preferable now that the reasoning floor makes single answers expensive. Either way, the docs should say that a tool-loop turn is re-checked between iterations, not inside one provider stream.

**Touches.** `internal/service/chat.go`, `internal/service/chat_stream_control.go`, `internal/service/chat_resume.go`, `internal/service/ai_budget_redis.go`, `internal/service/chat_budget_concurrent_test.go`, `docs/architecture/operations.md`, `docs/architecture/ai-providers.md`

**Done when.**

- With a daily budget of $1 and a `FakeProvider` emitting 50 delayed text parts, raising the recorded usage to $2 after the 10th part ends the stream with `AI_BUDGET_EXCEEDED` before the 50th. With the Redis ledger (miniredis) marked degraded mid-stream, it ends with `AI_BUDGET_UNAVAILABLE` within one re-check interval
- A test pins whether a plain stream is re-checked (or, if the docs option is chosen, that it is not), so the docs claim is executable; alias (Foundry) plain streams are never cancelled by the re-check
- A plain stream under budget makes no usage reads beyond the per-turn memo

### R-budget-10

**Azure AI Foundry spend has no bound of any kind in baki, while the org test route's swagger promises budget gating** · tier 3

**Problem.** Every gate exempts alias-model providers:

- The audited hook's `admit` returns nil for them.
- The chat start check and the loop checks return nil.
- `inFlightPricing` reserves zero.
- The knowledge gate skips an alias embedder.
- `pricingFor` returns a $0 "found" price for any `PricingUnpriced` spec, so usage rows record tokens at `EstimatedCost` 0.

Nothing else prices this spend either. `annotateServedModel` looks up the served model's catalogue entry for its windows but deliberately copies no price. Hosted agent tool steps are only counted in a metric described as "Not priced". `ReservationPricer` exists but nothing uses it.

The gap is Foundry-specific. The other org-shareable providers (`claude`, `openai`, `gemini`, `glm`, `xai`, `github-models`) are all budget-gated. But a Foundry-only tenant has no cap at all, and an org whose shared credential is Foundry has an unbounded share. ADR 0028 records the exemption but decides no alternative bound. Its wording "neither priced nor counted" is also loose, since rows are counted at $0.

Meanwhile the swagger for `POST /api/orgs/{id}/providers/azure-foundry/test` contradicts itself. It first says baki's daily budget does not count Azure usage, then says "a model test is also gated on the organisation's daily budget (AI_BUDGET_EXCEEDED / AI_BUDGET_UNAVAILABLE)". In fact `AzureModelService.Test` only calls `admitProbe` → `factory.Admit`, which applies the provider policy alone.

**Evidence.**

- `internal/ai/audited.go:133` — `admit`: a nil budget or `UsesModelAliases` → admitted (133-135); `pricingFor`: a `PricingUnpriced` spec → `Pricing{}, true` (606-608), so tokens are recorded at $0
- `internal/service/chat.go:88` — `enforceTurnBudget`/`enforceTurnBudgetMemo` return nil for alias providers (88-101); `inFlightPricing` returns `Pricing{}` for them (55-58)
- `internal/rag/service.go:143` — `checkBudget` skips an alias embedder (143-148)
- `internal/ai/served_model.go:222` — `annotateServedModel` looks up `catalogEntryFor(providerID, served)` for limits only: "No price: alias providers are unpriced" (222-235); `ReservationPricer`: "The chat service does not consult it today" (245-252); `metrics.go:95-101` counts hosted tool steps as "Not priced"
- `internal/api/org_azure.go:144` — the swagger says "a model test is also gated on the organisation's daily budget (AI_BUDGET_EXCEEDED / AI_BUDGET_UNAVAILABLE)", but `admitProbe` (`azure_models_runtime.go:850-855`) calls only `factory.Admit`, which is `admitProvider` (`policy.go:28-35`), the provider policy
- `docs/adr/0028-ai-budget-hook-and-provider-policy.md:11` — "Alias-model providers (Azure Foundry) are exempt (neither priced nor counted)"; `spec_azure_foundry.go:34` sets `Pricing: PricingUnpriced`
- `internal/ai/spec_claude.go:9` — `CapOrgShared` is also set on `claude`; likewise `spec_openai.go:8`, `spec_gemini.go:8`, `spec_glm.go:8`, `spec_xai.go:8` and `spec_github_models.go:9`, all of them budget-gated

**Change.** Narrow this item to the documentation fix:

- Correct the `org_azure.go:144` swagger and the generated `docs/docs.go` and `docs/swagger.yaml`: drop the budget-gating claim for model tests, and say the probe is gated by provider policy only.
- Tighten ADR 0028's wording: tokens are counted at $0, and there is no budget gate.

A bound for alias providers is a separate proposal, and it first needs an ADR 0028 addendum. It could take either or both of two forms:

- **A "priced by served model" mode.** When the served model, taken from the registry entry or the `recordServedModel` memo, has a catalogue row, price the usage row at that row's rates and reserve at the same rate via `ReservationPricer`. For priced deployments, drop the alias exemption in `admit`, `enforceTurnBudget` and `checkBudget`. The registry entry can carry an optional admin-entered price table.
- **A non-USD daily quota per scope/org.** For example `settings.AI.AliasDailyTurns` / `AliasDailyHostedSteps`, or daily input+output tokens. It is enforced through the same `AIBudget` seam, with `BudgetReason` `quota` and wire code `AI_BUDGET_EXCEEDED`. It is counted through the existing usage rows (tokens are recorded even at $0) and the ledger (unit count).

**Touches.** `internal/ai/audited.go`, `internal/ai/budget.go`, `internal/ai/served_model.go`, `internal/ai/spec_azure_foundry.go`, `internal/service/ai_budget.go`, `internal/service/chat.go`, `internal/rag/service.go`, `internal/api/org_azure.go`, `docs/swagger.yaml`, `docs/docs.go`, `core/models/settings.go`, `docs/adr/0028-ai-budget-hook-and-provider-policy.md`, `docs/architecture/ai-providers.md`

**Done when.**

- Follow-up proposal, priced mode: a Foundry deployment whose served model is `claude-*` produces a usage row with `EstimatedCost` equal to the catalogue rate × tokens, and `GetDailyUsage` includes it. With `DailyBudget` 1.00 and 1.00 spent, a chat turn on it is refused `AI_BUDGET_EXCEEDED` before the request leaves the process
- Follow-up proposal, quota: with an alias quota set and exhausted, the next Foundry turn is refused with `reason=quota` before the provider is called
- Either the org Azure model test is gated (returns `AI_BUDGET_EXCEEDED` over budget), or its swagger no longer claims it; the swagger golden pins the chosen text
- With no alias price or quota configured, the existing behaviour is unchanged (the alias exemption tests still pass)

**ADR.** ADR 0028 item 1 decides the exemption and no replacement bound. The org test route's budget claim (`org_azure.go:144`) is specified nowhere and is not implemented.

### R-budget-13

**'Today' is the Postgres session timezone, undocumented, inconsistent with the analytics day, and the UI promises a 'midnight UTC' reset nothing guarantees** · tier 3

**Problem.** `GetDailyUsage` sums rows with `created_at >= date_trunc('day', now())` and no `AT TIME ZONE`. The budget therefore resets at the database server's midnight: UTC on managed Postgres, and whatever the session TZ is elsewhere. `LoadUsageAnalytics`, by contrast, groups days in the caller's TZ. Most deployments run Postgres in UTC, so the risk today is an undocumented, unpinned boundary rather than a wrong day. Where the session runs at UTC+8, though, an operator sees the cap reset mid-morning and the dashboard's "today" disagree with refusals, and a harness cannot compute when a refused job may resume.

The UI makes a promise the backend does not. Both the budget toast and the budget bubble say the budget "resets at midnight UTC". The refusal message carries only the $x/$y amounts, and `budgetDetail` parses just those, so nothing on screen says when the user can actually continue. Nothing pins the boundary either: the fake backend returns a constant, and no test spans midnight or a non-UTC session.

**Evidence.**

- `internal/storage/database/postgres_users.go:1026` — `created_at >= date_trunc('day', now())` in the session TZ (org branch 1026, user branch 1034)
- `internal/storage/database/postgres_migrations.go:2056` — the covering indexes `usage_metrics_{user,org}_created_cost_idx` (2056-2057) were added "for an index-only GetDailyUsage" (2048)
- `internal/storage/database/postgres_analytics.go:259` — `tz := w.TZ`, default `UTC`; `(u.created_at AT TIME ZONE tz)::date` (259-268)
- `internal/testutil/fakebackend.go:1484` — `GetDailyUsage` returns a constant (1484-1486)
- `frontend/src/i18n/en.ts:1231` — `budgetBody`: "The budget resets at midnight UTC"; `budgetBubble` at 1233 repeats it
- `frontend/src/lib/chatErrors.ts:25` — `budgetDetail` parses only the $x/$y amounts from the message (25-28); no reset time reaches the UI
- `internal/service/ai_budget.go:223` — the `AI_BUDGET_EXCEEDED` message carries amounts only, no reset instant

**Change.** Pin the boundary in SQL so the UTC promise holds, and keep the indexed column bare so the covering indexes stay usable: `created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`. Add a `DATABASE_URL` test with the session `TimeZone` set to a non-UTC zone. Document the boundary in ADR 0028.

Optionally, add `resetsAt` to the `AI_BUDGET_EXCEEDED` details and to the budget status of [R-budget-14](budget.md#r-budget-14). Render it in the user's local time in the toast and the bubble, and drop the hard-coded UTC literal. A configurable `AI.BudgetTimeZone` (IANA name, default UTC, validated) is worth adding only if an operator asks for it. If it is added, it must keep the column bare too: use `created_at >= date_trunc('day', now(), $tz)` (the three-argument form), never `(created_at AT TIME ZONE $tz) >= …`.

**Touches.** `internal/storage/database/postgres_users.go`, `internal/storage/interfaces/usage.go`, `core/models/settings.go`, `internal/service/ai_budget.go`, `internal/api/handlers_chat.go`, `frontend/src/i18n/en.ts`, `frontend/src/lib/chatErrors.ts`, `frontend/src/components/chat/hooks/stream-engine/events.ts`, `docs/adr/0028-ai-budget-hook-and-provider-policy.md`

**Done when.**

- `DATABASE_URL` test: with session `TimeZone='Asia/Tokyo'` and the boundary at UTC, a row stamped 23:30 UTC yesterday is excluded from today's sum
- If `resetsAt` is adopted: the refusal error details include `resetsAt`; the bubble and the toast show that time in local format, and no "UTC" literal remains in the budget copy keys of any locale
- If `resetsAt` is adopted: the refusal message and the status endpoint carry the same `resetsAt` instant

**ADR.** ADR 0028 defines the daily budget; the day boundary and how it is exposed to the UI are unspecified.

### R-budget-14

**No budget status endpoint or client pre-send visibility; chat budget refusals arrive on SSE after a 200; the knowledge 429 carries no Retry-After** · tier 3

**Problem.** `POST /api/chat/stream` answers 200 with the stream id before `prepareTurn` runs the start check inside the worker. So `AI_BUDGET_EXCEEDED` and `AI_BUDGET_UNAVAILABLE` are visible only on the event stream, and the swagger lists no budget 429 on the POST.

There is no budget status to read:

- No route reports the limit, in-flight spend, remaining amount or reset time for a scope or org on the budget's own day boundary. No route registration mentions budget, usage or spend, and `GET /api/dashboard/analytics` reports daily AI spend history only.
- The client never asks. `chatApi` has no budget read, and the stream engine handles `budget` only after a refused create or a stream error.
- The knowledge routes answer 429 `BUDGET_EXCEEDED` without `Retry-After`, unlike `EMBEDDING_RATE_LIMITED`.

The first a user hears of an exhausted or unreadable budget is the refusal of a send. The client keeps the question and adds an error bubble. The server never stores the turn, because its start check runs before the user turn is persisted. Users on a shared org budget cannot see what is left before starting a long tool loop. A harness cannot pre-flight a batch, must parse SSE to learn it was refused, and cannot schedule a resume. The reservation and the start check already run before any spend, so the check can be moved before the 200.

**Evidence.**

- `internal/service/chat.go:1006` — `return streamID, nil` after the goroutine is launched; `prepareTurn` runs inside the worker at 854 and the start check at 551; the user turn is persisted after it, at 867
- `internal/api/handlers_chat.go:42` — `@Success 200 "Stream ID"`; a 429 is documented only for `CHAT_CAPACITY_REACHED` (42-45); the budget codes appear only on the SSE error payload (37)
- `internal/api/org_knowledge.go:135` — 429 `BUDGET_EXCEEDED` without `Retry-After` (135-136), versus `EMBEDDING_RATE_LIMITED` with it (168-170)
- `internal/api/route/resolver.go:176` — the only "budget" mention under `internal/api/route` is this comment; no `rb.Get`/`rb.Post` registration in `internal/api` matches budget, usage or spend
- `internal/api/handlers_dashboard.go:141` — `GET /api/dashboard/analytics` reports spend history, not the limit, in-flight spend, remaining amount or reset time
- `frontend/src/api/chat.ts:70` — `chatApi` has no budget read; `lifecycle.ts:285-288` and `events.ts:94-97` handle `budget` only on a failed create or the stream's error event, and keep the user message (only capacity and `modelNotAvailable` call `withdrawUserMessage`, `lifecycle.ts:263-269`)
- `internal/service/chat.go:545` — the reservation and the start check run before any spend (545-553), so they can move before the 200

**Change.** Add `GET /api/ai/budget?org=`. It returns `{dailyBudget, perUserDailyBudget, usedToday, inFlight, remaining, resetsAt, unavailable, scope}` from `AIBudget` (one `GetDailyUsage` plus `others()`), is capability-gated via `capability.Of`, and is absent in desktop mode.

Run the start check synchronously in `StreamChatMessage` before answering. An exhausted budget then becomes a 429 `AI_BUDGET_EXCEEDED` with `Retry-After` set to the seconds until `resetsAt`; keep the SSE path for mid-loop stops. The move is possible but not free. It means building the provider synchronously (`turnProvider`, `chat.go:532`) and reserving against the `ctl` that `tryStartStream` registers (`chat.go:719`); today the provider build and the reservation both run in the worker.

Set `Retry-After` on the knowledge `BUDGET_EXCEEDED`. On the client:

- Fetch the budget with the provider listing (same epoch), and again after every done or error that carries usage.
- Show the remaining amount in `ChatInput` next to the usage meter.
- At zero, disable Send with an accessible reason; under 10 %, show a warning chip.

**Touches.** `internal/api/handlers_chat.go`, `internal/api/handlers_ai_budget.go`, `internal/api/route/route.go`, `internal/service/chat.go`, `internal/service/ai_budget.go`, `internal/api/org_knowledge.go`, `internal/api/testdata/route_policy*.golden`, `docs/swagger.yaml`, `frontend/src/api/chat.ts`, `frontend/src/components/chat/hooks/useProviderSetup.ts`, `frontend/src/components/chat/ChatInput.tsx`, `frontend/src/components/chat/ChatUsageMeter.tsx`

**Done when.**

- Over budget, `POST /api/chat/stream` returns 429 with code `AI_BUDGET_EXCEEDED` and `Retry-After`, and no stream is created (`activeStreams` empty); a knowledge upload over budget returns 429 with `Retry-After`
- `GET /api/ai/budget` for an org member returns `remaining == dailyBudget − usedToday − inFlight` and `resetsAt` at the configured day boundary; with the ledger unreadable, the body says `unavailable: true`; the route policy and authz goldens include the route with chat scope
- `ChatInput` test: remaining 0 disables Send with an accessible reason; after an `AI_BUDGET_EXCEEDED` error the budget is refetched once

### R-budget-15

**The usage row's UserID is always the chain's scope even when the gate charged another scope** · tier 3

**Problem.** `admit` resolves the spender from `WithBudgetScope` (scope and org), and the per-user sub-limit gates that scope. The usage row does not follow:

- `usageMetric` writes `UserID: p.scope`, the chain's scope.
- Chat and Stream rows take `req.OrgID`, not the spender's org.
- `Embed` takes only the org from `spender()`.

The `admit` comment's claim that this is "the same resolution the usage row makes" is therefore false for both fields. `WithBudgetScope` accepts any scope, so the API lets a caller name a different scope than the chain's. Every caller passes the same one today, so the scope half is latent. But a future org-level automation running on a shared credential under a service scope would gate one user and bill another. The existing test `TestAudited_BudgetScopeFromContextBeatsTheChain` gates a Chat on `org-8` with `Request.OrgID` `org-1` but never checks the row, which would say `org-1`.

**Evidence.**

- `internal/ai/audited.go:136` — `admit` charges the `spender()` scope (136-137); `spender` resolves `WithBudgetScope` first (150-158); the comment at 129-131 claims the usage row uses the same resolution
- `internal/ai/audited.go:535` — `UserID: p.scope` on the row (533-535); Chat and Stream record `req.OrgID` (184, 253); `Embed` uses only the org from `spender` (663)
- `internal/ai/budget.go:50` — `WithBudgetScope` accepts any scope (50-52)
- `internal/ai/audited_budget_test.go:209` — a Chat gated on the context's `org-8` with `Request.OrgID` `org-1`; the row is never checked (209-215)

**Change.** Resolve both `UserID` and `OrgID` from `spender()` in `record`/`usageMetric` (the context's budget scope when one is set), so the row records exactly what was gated. Add a test that a `WithBudgetScope` naming a different scope and org records that scope and org. Extend `TestAudited_BudgetScopeFromContextBeatsTheChain` to check the row.

**Touches.** `internal/ai/audited.go`, `internal/ai/audited_budget_test.go`

**Done when.**

- Test: with chain scope `u1` and `WithBudgetScope("svc", org)`, the usage row's `UserID` is `svc`
- Test: a Chat gated on the context's `org-8` with `Request.OrgID` `org-1` records a row with `OrgID` `org-8`

### R-budget-17

**Cached-input pricing models only Claude: OpenAI-family cached tokens are billed at full rate, Gemini cache counts are never parsed, and rows keep no breakdown** · tier 3

**Problem.** Cached input is priced correctly only for Claude:

- `cacheReadMultiplier` returns 1.0 for every non-Claude route. The OpenAI SSE and Responses parsers carry the `prompt_tokens_details.cached_tokens` slice, but it is priced at the full input rate. OpenAI bills gpt-4o cached input at 50 % and the gpt-5 family at 10 %.
- The Gemini parsers read `promptTokenCount`, `candidatesTokenCount` and `thoughtsTokenCount` but not `cachedContentTokenCount`, which appears nowhere in `internal/ai`. Gemini caching is therefore invisible.
- The usage row stores `PromptTokens` as the cache-inclusive total, with no `cache_read`/`cache_write` columns, so analytics cannot reconcile `estimated_cost` with tokens.

Over-counting is the safe direction for the budget, but it is wrong on the dashboard and in the done payload's rates.

**Evidence.**

- `internal/ai/catalog.go:163` — `cacheReadMultiplier`: `!claudeRoute` → 1.0 (163-167); only Fable/Mythos/Opus rates are modelled (174-180); `cacheWriteMultiplier` is a constant 1.25 at 186
- `internal/ai/catalog.go:34` — the direct OpenAI catalogue lists only `gpt-4o`, `gpt-4o-mini` and `gpt-4-turbo` (34-38)
- `internal/ai/stream.go:636` — the OpenAI SSE parser reads `prompt_tokens_details.cached_tokens` (636-659); `responses.go:813-819` does the same for Responses
- `internal/ai/stream.go:835` — the Gemini `usageMetadata` struct has no `cachedContentTokenCount` (835-841); `gemini.go:231-238` and 419-422 likewise; a grep over `internal/ai` finds none
- `internal/ai/audited.go:523` — `inputCost = (uncached + readMul×cacheRead + 1.25×cacheWrite) × InputCostPerM` (523-525); only Claude-wire paths set `CacheWriteTokens` (`claude.go:701`, `stream.go:362-363`)
- `internal/ai/audited.go:533` — `UsageMetric` has `PromptTokens`/`CompletionTokens`/`EstimatedCost` only (533-547; `usage.go:15-26`); `postgres_users.go:1003-1008` inserts no cache columns
- `internal/ai/spec_claude.go:3` — "the only provider billing prompt-cache reads and writes at their own rates", the assumption the code encodes

**Change.** Carry cached-input rates per catalogue row, or in a per-provider multiplier table (`openai` 0.5 for the gpt-4o family and 0.1 for the gpt-5 family, xAI and GLM per vendor), and look them up by served model. The table must cover xAI and GLM because they share the OpenAI-wire parsers, which also carry `cached_tokens`.

The direct OpenAI catalogue lists only `gpt-4o`, `gpt-4o-mini` and `gpt-4-turbo`, which has two consequences:

- A gpt-5-family rate has no catalogued direct-OpenAI model to apply to. An uncatalogued gpt-5 falls back to the provider default, gpt-4o pricing.
- `gpt-4-turbo` gets no cache discount.

Parse Gemini `cachedContentTokenCount` into `CacheReadTokens`. Add `cache_read_tokens`/`cache_write_tokens` to `usage_metrics` with an expand migration, and surface them in analytics. The concern that the 1.25 write multiplier applies to non-Claude providers is theoretical: only Claude-wire paths set `CacheWriteTokens`.

**Touches.** `internal/ai/catalog.go`, `internal/ai/audited.go`, `internal/ai/stream.go`, `internal/ai/gemini.go`, `internal/storage/database/migrations/`, `internal/storage/interfaces/usage.go`, `internal/service/dashboard_analytics.go`

**Done when.**

- A gpt-4o call with 1 000 cached of 2 000 prompt tokens records 1000×2.5 + 1000×1.25 per million (test)
- A Gemini stream reporting `cachedContentTokenCount` carries it on the Done chunk's `CacheReadTokens`
- `usage_metrics` rows store the cache breakdown, and the analytics by-model table can show it

### R-budget-c10

**The desktop has no spend accounting: no usage store, so a daily budget counts only concurrent in-flight spend; there is no budget control and no 'spent today', while the settings …** · tier 3

**Problem.** On the desktop the storage backend is nil. `ProvideAI` leaves the usage recorder nil. `ProvideAIBudget` passes the nil backend to `NewAIBudget` as its `DailyUsageReader`, so `dailyUsage` always returns $0. A `dailyBudget` therefore trips only on the turn's own not-yet-recorded spend plus other running streams (`usage + inflight + concurrent`, with usage at 0). No completed spend is ever counted, so there is no daily cap and no cost history at all.

A desktop user cannot set a budget anyway. `AIBehaviorPanel`'s "History & Costs" block holds only the save-history switch, and the `dailyBudget`/`showCosts` strings are orphaned. The cloud org settings section has no budget field either.

Meanwhile the same panel always shows the "Embedding Assistant" and "Embedding Model" controls. Yet the knowledge base routes are mounted only with the server-store capability (`internal/api/org_knowledge.go:366`), which the desktop profile lacks (`docs/architecture/modes.md:7`).

**Evidence.**

- `internal/di/services.go:123` — the recorder is left nil when `backend == nil` (desktop): no usage rows
- `internal/di/services.go:245` — `ProvideAIBudget`: "backend is nil on the desktop (no usage store: spend reads $0, so only the in-flight ledger can trip there)"
- `internal/service/ai_budget.go:181` — `dailyUsage` returns `(0, nil)` without a usage reader (180-182)
- `internal/service/ai_budget.go:220` — `total = usage + inflight + concurrent`: with usage at 0, only the turn's own reservation and live streams count
- `frontend/src/components/settings/AIBehaviorPanel.tsx:126` — the History & Costs section renders only the `saveHistory` switch (126-143), with no budget or spend field; the embedding controls are rendered with no mode check (90-124)
- `frontend/src/i18n/en.settings.ts:1005` — `dailyBudget`/`dailyBudgetHint` (and `showCosts` at 1003) are strings with no consumer

**Change.** Tier 3: this is polish, not a PAD capability. Give the desktop profile a small file-backed usage ledger in the config directory, either `usage/<yyyy-mm-dd>.jsonl` or a daily aggregate written atomically and kept for N days. Implement both `ai.UsageRecorder` and `DailyUsageReader` on it. Wire them in `ProvideAI` and `ProvideAIBudget` when `capability.Of(cfg)` lacks `server-store`, not on `backend == nil` (`docs/architecture/README.md:37`, `modes.md:11`), so the existing gate enforces a real daily cap. Add a local-backend-only "Daily AI budget" field and a read-only "Spent today" line to `AIBehaviorPanel`, fed by a small endpoint on the same ledger. Hide the embedding controls where the knowledge base is unavailable.

**Touches.** `internal/di/services.go`, `internal/service/ai_budget.go`, `internal/storage`, `internal/api/handlers_system.go`, `frontend/src/components/settings/AIBehaviorPanel.tsx`, `frontend/src/types/system.ts`, `frontend/src/i18n/en.settings.ts`

**Done when.**

- Desktop test: after recorded completions totalling $X today, `AIBudget.CheckBudget` with `DailyBudget` < X returns `ErrAIBudgetExceeded` with no stream in flight
- The desktop ledger survives a backend restart and rolls over at the documented day boundary
- On a local backend, `AIBehaviorPanel` shows an editable daily budget and today's spend; on cloud it shows neither
