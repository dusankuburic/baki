# Improvement Roadmap

A prioritized backlog from a codebase deep-dive, scoped to three themes: **frontend
architecture**, **backend code quality / performance**, and **security hardening**.
Every item was verified against source. Items the audit raised that turned out to be
already-handled or deliberate are recorded under *Investigated & rejected* so they
aren't re-opened.

Confidence: ✅ verified in source · ⚠️ plausible, confirm exact scope while implementing.

Status is tracked inline — update the checkbox as items land.

---

## Tier 1 — Quick wins (low effort, low risk)

- [x] **S1 — Set `MaxHeaderBytes` on the HTTP server** ✅ _security_
  `http.Server` (`main.go`) set timeouts but not `MaxHeaderBytes`. Made the 1 MB
  header-bomb guard explicit. Body was already capped at 10 MB (`router.go`,
  `http.MaxBytesReader`).

- [x] **S2 — Fix the (silently broken) Go lint job + expand linters** ✅ _security + quality_
  **Root cause found:** CI's `Go Lint` job has been failing on every recent run —
  `golangci-lint-action@v6` installs golangci-lint **v1** (built with go1.24), which
  aborts on this **go1.25** module ("language version too low"). The linter never
  actually ran, so a backlog accumulated unseen.
  **Fix:** migrated `.golangci.yml` to the **v2** schema (v2 supports go1.25; folds
  `gosimple` into `staticcheck`, moves `gofmt` to `formatters`), bumped the action to
  `@v7` (both root + `core/` invocations), and added the new linters: `gosec`,
  `errorlint`, `bodyclose`, `contextcheck`, `nolintlint`. Test files and dev `scripts/`
  are excluded for `errcheck`/`gosec` noise. The G202 false positive in
  `postgres_users.go` (safe `$N` placeholder build) is annotated with `#nosec`.
  Adopted `only-new-issues: true` so the linter now **gates new code** without forcing
  a one-shot cleanup of the inherited backlog (~107 findings root + ~32 core) — see B4.

- [x] **B1 — Scrubber: replace JSON deep-copy with typed clone** ✅ _perf_
  `ScrubDocument` (`core/ai/scrubber/scrubber.go`) did a full
  `json.Marshal`→`json.Unmarshal` round-trip per AI request just to deep-copy before
  masking — serializing the whole flow (up to 10k+ blocks) twice. Replaced with a
  typed deep clone, then the existing `scrubBlock` walk.

- [x] **F1 — Batch finding suppression** ✅ _frontend perf/UX_
  `analysisStore.suppressMany` fired one HTTP request per finding (100 findings = 100
  requests, no partial-failure handling). Added a batch backend path and a single
  batched client request with partial-failure handling.

---

## Tier 2 — Structural (medium effort)

- [x] **F2 — Split oversized view components** ✅ _frontend architecture_
  `components/layout/MainPane.tsx` (372 lines): owns editor-group state, drag/resize,
  and routing across ~8 top-level views → extract a `useEditorGroups` hook + a
  `<SystemViewRouter>`. `components/findings/FindingsTab.tsx` (399 lines): mixes
  orchestration, filter state, API calls, and 4 render modes → split into
  `<AnalysisRunner>`, `<FindingsView>`, `<FindingsDiffView>`. Mechanical extractions;
  do one at a time behind the Vitest suite.

- [x] **F3 — Graph-view render hygiene** ✅ _frontend perf_
  `GraphView.tsx` and `flow/ExecutionGraphView.tsx` recompute theme CSS vars via
  `getComputedStyle` every render and `remove()`+`add()` all Cytoscape elements on
  each data change. Memoize CSS-var lookup (recompute on theme change only) and use
  `cy.batch(...)`.

- [x] **F4 — Centralize the flowStore reset cascade** ✅ _frontend architecture_
  `stores/flowStore.ts` `setDocument` reaches into search/analysis/chat/editor stores
  to clear them on flow switch. Introduce a single `resetForNewFlow()` coordinator (or
  document the contract) so the cross-store dependency is explicit and testable.

- [x] **B2 — DRY the auth register/login handlers** ✅ _backend quality_
  `internal/api/handlers_auth.go` repeats decode + email/password validation across
  register and login. Extract a `decodeCredentials`/`normalizeCredentials` helper.
  Covered by existing `handlers_auth_test.go`.

- [x] **B3 — Cap the flow search-index cache** ✅ _backend perf/memory_
  `internal/service/flow.go` `idxCache map[string]*search.SearchIndex` has no size
  bound; a long-lived cloud process touching many flows grows unbounded. Reuse the
  existing LRU in `core/cache/lru.go` (cap ~256).

---

- [x] **B4 — Burn down the inherited lint backlog** ✅ _quality_
  Cleared the full backlog (~107 root + ~32 core) so the linter now gates the whole
  tree; `only-new-issues` removed from CI. Work done:
  - `gofmt`/`staticcheck` quickfixes via `golangci-lint --fix`; `errorlint` converted
    `==`/`!=`/`switch` on errors to `errors.Is` (added missing `"errors"` imports).
  - `errcheck`: idiomatic ignores (`sql` Close/Rollback, websocket conn methods,
    `fmt.Fprint*`, `io.WriteString`) moved to `errcheck.exclude-functions`; remaining
    real cases handled inline (`_ =` / explicit handling).
  - `gosec`: tightened internal dir/file perms to `0750`/`0600` (storage, logger,
    conversations, history, demo); annotated documented false positives with
    `#nosec <rule> -- reason` (public OAuth IDs/URLs, jitter RNG, operator/CLI file
    paths, file-reveal subprocess, detached-goroutine contexts, placeholder-only SQL
    concatenation, range-bounded slice index); disabled redundant `G104` (errcheck
    already covers it).
  - **Bug found & fixed:** `golangci-lint --fix` had rewritten `parser.computeIndent`
    from `if/else { break }` into `switch { default: break }`, silently changing it to
    count *all* whitespace instead of leading indent (a switch `break` exits the
    switch, not the loop). Restored correct semantics via `default: return n`; caught
    by the parser golden-file tests.
  - **contextcheck dropped** (not enabled): its findings are all intentional patterns
    (ctx-less settings/parser APIs, detached logging/ws/cleanup contexts). Threading
    `ctx` through those public APIs is a separate refactor, tracked below as B5.

- [x] **B5 — (optional) thread `context.Context` through settings/parser APIs** ✅ _quality_
   Scoped: `context.Context` threaded through `SystemService`'s settings methods + handler
   call sites so request cancellation propagates to the DB (previously dropped via
   `context.Background()`). The core parser (pure CPU, leaf, CLI-shared) is intentionally
   left ctx-less; `contextcheck` stays disabled per B4 (remaining findings are intentional
   detached cache/logging/ws/cleanup contexts).

## Tier 3 — Polish / discretionary

- [x] **S3 — Tune scrubber entropy threshold** ✅ _security, debatable_
  `scrubber.go` masks values with Shannon entropy `> 4.0`; consider length-tiered
  thresholds (len>50 ⇒ >3.5, len>20 ⇒ >4.0). Treat as an **experiment** — measure
  false-positive/negative against the scrubber corpus first; over-masking degrades AI
  answer quality.

- [x] **S4 — Container image scanning** ✅ _security CI_
   Added a `trivy image` job to `.github/workflows/deploy.yml` that fails the deploy on
   HIGH/CRITICAL findings (with a fix available); the deploy job now `needs: [build-and-push, scan]`.

- [x] **F5 — Type-safety hardening for backend responses** ✅ _frontend_
   Added `parseChatMessage`/`parseChatMessages` boundary validators (`lib/chatMessage.ts`)
   that replace the unchecked `as ChatMessage` casts in `useChatConversations.ts`, dropping
   malformed entries and coercing unknown roles. The `reports.get(doc.id)` lookups were left
   as-is — they are already guarded with optional chaining + doc-null checks.

---

## Tier 4 — AI integration deep dive

A follow-up audit scoped to the AI/LLM integration (backend provider layer, chat
service, scrubber, cost auditing; frontend chat hooks, API client, privacy UI). No bugs
found — the architecture is deliberately hardened (circuit breaker → retry → tracing →
cost-audited decorator chain per provider, scrubbing before every LLM call, race-guarded
chat streaming on the frontend). Six items surfaced; the four quick wins below are done.

- [x] **F6 — Surface AI data-scrubbing disclosure in `PrivacyPanel.tsx`** ✅ _frontend privacy_
  `PrivacyPanel.tsx` documented API-key storage but never mentioned that flow content is
  scrubbed for secrets/PII (`core/ai/scrubber/scrubber.go`, invoked via
  `ChatService.buildScrubbedContext`) before being sent to any AI provider. Added a
  disclosure card matching the existing style.

- [x] **F7 — Accessibility: progress indicator in `AnalysisRunner.tsx`** ✅ _frontend a11y_
  The "Analyzing... X%" text had no ARIA semantics. Added `role="progressbar"`,
  `aria-valuenow`/`aria-valuemin`/`aria-valuemax`, and `aria-label` so screen readers
  announce progress.

- [x] **F8 — Dedupe the 401-refresh-retry logic in `client.ts`** ✅ _frontend quality_
  The same "check 401 → dedupe via `refreshInFlight` → `invalidateConfigCache()`" block
  was repeated in `request`, `requestBlob`, and `connectEvents`. Extracted a shared
  `refreshOnUnauthorized()` helper; each call site keeps its own status guard and
  try/catch since post-refresh behavior differs (inline retry vs. reconnect/backoff).

- [x] **B6 — Chat-specific request body cap in `handlers_chat.go`** ✅ _backend security/cost_
  The global 10 MiB `http.MaxBytesReader` (`router.go`) had no smaller override for chat,
  so a single message could be up to 10 MiB before scrubbing/tokenization. Added
  `maxChatMessageBodyBytes = 1 MiB`, applied in `handleStreamChatMessage` and
  `handleSaveConversation`, following the existing per-handler cap precedent in
  `handlers_org.go` (`maxKnowledgeUploadBodyBytes`).

- [x] **F9 — Client-side request timeout** ✅ _frontend reliability_
  `client.ts`'s `request`/`requestBlob` had no `AbortController`/timeout — only the SSE
  `connectEvents` path did, so a hung backend call could block the UI indefinitely. Added
  an `AbortController`-based timeout in `doFetch` (30s default for `request`, 90s for
  `requestBlob` since account-export blobs can be larger), converting an abort into a
  clean `Error` with the original `AbortError` preserved via `cause`. The few genuinely
  slow endpoints — bulk flow upload/folder-load (`flow.ts`), folder-wide batch analysis
  (`analysis.ts`), and PDF/Markdown export (`export.ts`) — pass an explicit 90s–300s
  override at their call site rather than being bound by the default.

- [x] **B7 — Observability for pricing-catalog fallback** ✅ _backend observability_
  `audited.go` logged a warning when a model isn't in the pricing catalog and falls back
  to provider-default pricing, but it was log-only — catalog drift (e.g. a new model ID)
  was invisible to alerting. Added `pad_ai_pricing_fallback_total{provider,model}`
  (`internal/metrics/metrics.go`) and call `metrics.RecordPricingFallback(providerID,
  modelID)` alongside the existing log in `audited.go`'s `record()`.

## Tier 5 — Chat streaming deep dive

A follow-up audit specifically verifying whether AI chat responses are genuinely
streamed token-by-token end-to-end (provider → backend → browser) or simulated/batched
somewhere in the pipeline, and reviewing how the frontend reacts to backend chat
messages during that workflow.

**Verdict: streaming is real, not simulated, at every hop.** Traced provider HTTP SSE
body → `bufio.Scanner`-based incremental parse (`internal/ai/stream.go`) → per-chunk
callback in `StreamChatMessage`/`runToolLoop` (`internal/service/chat.go`) → immediate
per-event `Fprintf`+`Flush` (`internal/api/events.go`'s `HandleEvents`) → frontend
`onChunk` per SSE event (`useStreamingMessage.ts`). The `requestAnimationFrame`
coalescing in `useAIChat.ts` only merges same-frame chunks for render smoothness and
cannot reveal text ahead of arrival. The only full-buffer behavior is `ResumeStream`
(`chat.go`) / `onReplace` (`useStreamingMessage.ts`), a deliberate reconnect/catch-up
path — verified it overwrites rather than duplicates accumulated text.

- [x] **B8 — `beginStream` failure leaked a backend stream for up to 5 minutes** ✅ _backend/frontend correctness_
  `ChatService`'s `awaitStart()` (`internal/service/chat.go`) blocks the provider-stream
  callback until `POST /api/chat/begin` arrives or the stream's 5-minute
  `maxChatStreamDuration` context expires — intentional, so chunks aren't dropped before
  the frontend subscribes. But `useAIChat.ts`'s `executeSend` had three cleanup paths
  after creating a stream, and only two called `chatApi.cancelStream` (the
  stale-generation guard and `handleCancelStream`); the catch block around
  `await registerStream(sid)` — exactly where a `beginStream` failure surfaces — tore
  down only the frontend's SSE listeners, never cancelling the backend stream. Every
  `beginStream` failure (dropped connection, backend restart between stream-create and
  begin, etc.) left the goroutine and its live provider connection running uselessly for
  up to 5 minutes. Fixed by hoisting `sid` out of the `try` block and calling
  `chatApi.cancelStream(sid)` in the catch path, matching the existing pattern used
  elsewhere in the same function.

---

## Investigated & rejected (do NOT re-open)

- **N+1 in authz list endpoints** — `BatchFlowPermissions` already exists (`authz.go`)
  and is the path `handlers_library.go` / `service/library.go` take.
- **Chat stream has no deadline / leaks stream IDs** — `chat.go` already wraps with
  `maxChatStreamDuration`; the `AfterFunc` is a deliberate resume-retention grace
  window.
- **`analysisStore.findingsByBlock` is redundant derived state** — it is an
  intentional O(1) per-block index with documented eviction protection; computing it
  per-render would regress the 10k-block BlockView.
- **SSE sets wildcard CORS** — `events.go` already echoes ACAO only for allowlisted
  origins; current behavior is the safe default.

---

## Verification

```bash
# Backend
go test ./...
go test -race ./...
golangci-lint run

# Frontend
cd frontend && npm run test:run && npx tsc --noEmit && npm run lint
```
