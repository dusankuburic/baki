# Azure AI Foundry backend — leak/bug audit results + fix plan

Three parallel audits (goroutine/handle/cache-growth · races/error-paths · chat-service lifecycle) covered the full Azure path plus the just-landed fix round. **No P0.** The core streaming lifecycle (watchdogs, cancellation, resume, usage recording, singleflight, resp.Body closes on every path) verified hardened. But 4 significant defects and ~10 smaller ones were found, including **one bug introduced by the previous fix round**.

## Findings

**P1 — `renameOverOpenReader` retry never fires for sharing violations** (introduced last round; `internal/storage/filesystem/local_storage.go:79-92`). The predicate matches `fs.ErrPermission`, but Go maps only `ERROR_ACCESS_DENIED(5)/EACCES/EPERM` to it — a reader-held destination surfaces as `ERROR_SHARING_VIOLATION(32)`, which does NOT match, so the retry is dead code for its exact target case (verified against Go's `Errno.Is` source). Fix: also match errno 32 via a `//go:build windows` helper; keep the ErrPermission match.

**P1 — `breakerRegistry` "cap" is not a cap** (`internal/ai/circuitbreaker.go:66-94`). `pruneHealthyBreakersLocked` only removes closed+zero-failure entries; an insert proceeds even when pruning evicts nothing, and any entry with ≥1 failure persists for process lifetime. Keys are per user-endpoint × model × agent (up to 200 agents/project) → unbounded monotonic growth (10k–200k sticky entries at 1000 users). Fix: age-based eviction (closed entries idle > N×openDuration) + hard cap with oldest-`lastFailure` eviction after pruning.

**P2 — fix-apply keepalive goroutine leaks on panic, and a real panic path exists** (`internal/service/chat_fixes.go:160-176`, `:296-312`). `close(applyDone)` is not deferred and the select has no `ctx.Done()` arm — a panic in the apply window strands the ticker goroutine forever (pinning the streamCtl + accumulated answer buffer). Reachable: the batch path calls `s.analysisCache.AnalyzeFlowReadOnly` (~line 583) without the nil guard its single-fix sibling has (line 417). Fix: `defer close` + `defer Stop` + ctx arm + missing nil guard.

**P2 — `ProviderService.modelCache` grows without bound** (`internal/service/providers.go:41-46,100-120`). Entries expire for reads but are never removed from the map; account deletion cascades the DB rows but never invalidates the cache (the OnApiKeyDeleted hook is the only wiring). One `[]ModelInfo` per (user × provider) retained forever under churn. Fix: prune expired entries on insert; add `InvalidateScope(scope)` wired into account deletion (follow the existing DI hook pattern).

**P2 — embeddings split-retry amplification** (`internal/ai/openai_compat.go:612-624`). Worst case `2n−1` requests (199,999 for a 100k batch), and the whole cascade re-runs up to 3× because the outer RetryingProvider retries the aggregate on any sub-batch 429/5xx — actively worsening rate limits. The regex also matches per-input-too-large 400s, triggering doomed cascades. Fix: per-leaf bounded retry inside `embedBatch`, propagate final error non-retryable, total-request budget, tighten the rejection match to batch-count errors.

**P2 — parallel tool-exec goroutine has no recover → process crash on tool panic** (`internal/service/chat_tool_exec.go:90-98`). Model-controlled input feeds tool handlers; a panic in a bare goroutine kills the process and every in-flight stream. Fix: recover → error result.

**P2 — metrics cardinality: `pad_ai_pricing_fallback_total{model}` takes the raw client-controlled model string for non-alias providers** (`internal/metrics/metrics.go:243-249`, `internal/ai/audited.go:306`). On env-overridden compat endpoints every unique model string mints a permanent counter series. Fix: collapse uncatalogued models to a fixed label.

**P3s**: `postResponses` store-guard unreachable after 4 repairs (hoist above cap, `responses.go:176-187`); undecodable SSE item events bypass the keepalive (`responses.go:618,654` — the exact events the keepalive exists for); `responsesCallKey` added/done asymmetry can double-emit one tool call (canonicalize callID-first); content-filter terminal drops usage (attach tokens); `RetryingProvider.Stream` pre-delivery hold discards usage riding held error chunks (preserve/stash); `atomicWrite` fixed `.tmp` name can interleave concurrent settings writes (unique temp via `os.CreateTemp`, mirroring `conversation_store.go`); OAuth revoke paths skip model-cache invalidation + ai-level Foundry cache served on TTL after key delete; `thinkSplitter` accumulates unbounded whitespace in `<think>` detection (cap at 1 KB); unbounded `json.Decode` on 200 bodies in embeddings/GitHub/Copilot listing paths (32 MB LimitReader); Copilot singleflight keyed by raw token under first caller's ctx (key by hash, detach like Entra).

**Excluded (no change)**: RLS commit-after-response ordering (inherent to design, warn-logged); `watchStream` phantom last tick (log noise); global `modelGen` fencing breadth (efficiency only).

## Implementation — 3 parallel agents, disjoint file ownership, then my integration pass

**Agent A — internal/ai + internal/metrics**: all circuitbreaker, openai_compat (embeddings retry/budget + LimitReader), responses.go (4 P3 fixes), retry.go hold-usage, audited.go/metrics.go label collapse, stream.go thinkSplitter cap, copilot_auth.go singleflight hygiene. New/updated tests for each.

**Agent B — internal/service (+ DI wiring)**: chat_fixes.go defer/ctx/nil-guard (both sites), chat_tool_exec.go recover, providers.go cache pruning + InvalidateScope + revoke-path invalidations, account-deletion hook (following the OnApiKeyDeleted pattern in di/services.go). Tests.

**Agent C — internal/storage/filesystem**: sharing-violation predicate via build-tagged windows helper + unit test; unique-temp atomicWrite + concurrency stress test.

**Integration pass (me)**: `go build ./... && go vet ./...`, full `go test ./...` (root + core), `go test ./internal/ai/ ./internal/service/ -race`, review the combined diff, final report. No frontend changes required (backend-scoped audit). Nothing gets committed unless you ask.