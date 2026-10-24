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
