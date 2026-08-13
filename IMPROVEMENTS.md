# IMPROVEMENTS — Baki Deep-Dive Findings

Living tracker of findings from the codebase deep-dive. Status: `[ ]` pending, `[~]` in progress, `[x]` done.

---

## Phase 1 — Critical correctness

### Analyzer engine (core/analyzer, core/parser)

- [x] **C1** `rule_subflow_no_error_handler.go:49-56` — Finding sets no `BlockID`; dedup collapses multi-subflow findings (2nd silently dropped), inline suppression impossible, SARIF carries no line. **Fix:** set `BlockID: block.ID`; add regression test through `RunAnalysis` asserting N findings for N handler-less subflows.
- [x] **H1** `cache.go:51`, `incremental.go:154` — `FlowHash`/`computeSubflowHash` fold parser-minted UUIDs (`b.ID`) → cache never hits across re-parses. **Fix:** drop `b.ID` from both hashes; add stability test `FlowHash(parse(s)) == FlowHash(parse(s))`.
- [x] **H2** `extractors.go:222,224` — typo `MergeList` vs canonical `MergeLists`; `RemoveDuplicatesFromList` vs `RemoveDuplicateItemsFromList` → data-flow analysis blind to those actions. **Fix:** align with classifier spelling; table test covering every variable-manipulation prefix.
- [x] **H3** `rule_unhandled_error.go:14`, `rule_missing_retry.go:11`, `rule_missing_timeout.go:12`, `rule_sensitive_exposure.go:25` — prefix lists use `Http.`/`Ftp.` but canonical PAD module is `HTTPClient.`/`FTP.` → security/reliability rules miss real flows. **Fix:** align with classifier; regression test feeding `HTTPClient.*`.
- [x] **H4** `rule_error_swallow.go:93-96` — catch-all `BlockTypeAction` short-circuit marks any action child as "handler does something" → false negatives. **Fix:** drop the catch-all; test with unrelated placeholder action.

### Frontend

- [x] **C2** `api/client.ts:191-193` — every API response blindly `as Promise<T>` cast. **Fix:** add `zod`; migrate analysis/settings/auth endpoints behind a `requestValidated` wrapper.

---

## Phase 2 — Security hardening

- [x] **H10** `bakicli` (tracked binary, 5.2 MB) bypasses `.gitignore`. **Fix:** `git rm --cached bakicli`; add `/bakicli` to `.gitignore`. *(regressed in 53e10e3; re-fixed in Phase 3A with a CI guard preventing recurrence)*
- [x] **H11** `main.go:509-516`, `keystore.go:46-50` — `PAD_AUTH_SECRET` shared as JWT key AND AES keystore key. **Fix:** introduced `PAD_ENCRYPTION_KEY` (config + Bicep + .env.example); backward-compat fallback to auth secret with deprecation warning.
- [x] **M1** `auth/middleware.go:59` — case-sensitive `Bearer` parsing. **Fix:** case-insensitive scheme match (RFC 7235).
- [x] **M3** `middleware/ratelimit.go:304-306` — Redis limiter fails OPEN on outage. **Fix:** embedded per-replica in-memory fallback bucket; rate still enforced (degraded) on Redis error.
- [ ] **M4** `infra/main.parameters.json` — misleading placeholders; deploy workflow bypasses it. **Fix:** remove or fix. *(deferred — low value, workflow doesn't use it)*
- [x] **M5** `docker-compose.yml:31-32` — Postgres bound `0.0.0.0`. **Fix:** `"127.0.0.1:${DB_PORT:-5432}:5432"`.
- [x] **M6** `ci/azure-pipelines.pad-analysis.yml:47` — pins Go 1.23; project needs 1.25. **Fix:** bumped to `1.25`.
- [x] **M8** SSO takeover log leaks victim email at Info (`handlers_sso.go:272`). **Fix:** uses `redactEmail` + logs userID.
- [x] **I1/I2** No CODEOWNERS. **Fix:** added `.github/CODEOWNERS` for auth/api-middleware/infra/Dockerfile.
- [x] **I3** deploy workflow triggers on `master` only (`deploy.yml:5`). **Fix:** aligned with CI (watch `main` + `master`).
- [x] **L7** `npm audit` uses legacy `--production` + third-party pkg (`ci.yml:157`). **Fix:** `npm audit --omit=dev`.
- [x] **L10** `/metrics` IP-only guard. **Fix:** optional `PAD_METRICS_TOKEN` bearer (constant-time compared).

---

## Phase 3 — Storage bug fixes (interface split deferred)

- [x] **H6** Missing `rows.Err()`: `postgres_comments.go:45`, `postgres_sharetokens.go:64`. **Fix:** `return out, rows.Err()`.
- [x] **H7** Filesystem `ListFlows` returns directory order, not `updated_at DESC` (`local_storage.go:163-222`). **Fix:** collect → sort (mirror `flowOrderBy`) → offset/limit; +ordering/pagination regression test.
- [~] **H8** Filesystem silent-success stubs (`local_storage.go:774,798`). **Deferred** — stubs are documented intentional no-ops and internally consistent (save=no-op, load=empty/not-found); converting to errors risks breaking local-mode dashboard/refresh paths that call them. Interface split (Phase 3-deferred) is the durable fix.
- [x] **M6-index** Missing `(org_id, created_at)` index on `usage_metrics` (hot budget path). **Fix:** migration 7 `usage_metrics_org_created_idx`.
- [~] **M3-RLS** `BeginTx` paths bypass RLS undocumented (`postgres_orgs.go`, `SaveFlowVersion`, `SaveKnowledgeChunks`). **Partial fix:** `SaveFlowVersion` and `SaveKnowledgeChunks` now route via `BeginRLS`/`hasRLSTx` (the latter in Phase 3A); `SaveOrg`/`MutateOrg` still use raw `b.db.BeginTx` — `organisations`/`org_members` have no RLS policies so the security impact is nil, but the independent-commit footgun remains (a handler that errors after SaveOrg cannot roll back the org mutation).
- [x] **M9** Migration system has no checksum. **Fix:** added a `checksum TEXT` column to `schema_migrations` (additive `ADD COLUMN IF NOT EXISTS`), recorded as `sha256(step.sql)` in `applyMigration`. On every boot `verifyChecksums` (called from `migrate` right after reading the current version) re-hashes each already-applied step and **fails boot with a "schema migration drift detected" error** on any mismatch — i.e. an edited shipped migration. Pre-checksum deployments (empty checksum) are gracefully backfilled with the running binary's value rather than flagged as drift. +`TestMigrationChecksum_Deterministic` (no DB) and Postgres integration tests `TestMigrate_RecordsChecksums`, `TestMigrate_ChecksumDriftFailsBoot`, `TestMigrate_BackfillsEmptyChecksum` (podman harness).

---

## Phase 4 — Frontend reliability

- [x] **H9** `FindingCard.tsx` (was 459 lines, 6 concerns, zero tests). **Largely addressed** — extracted to 226 lines with `useRelatedFindings`/`useFindingComments`/`useFindingFix` hooks + a 210-line test file. Residual: still mixes render + several action surfaces, but the "untested + tangled" concern is closed.
- [x] **H1-fe** `focusedFindingKey` not cleared on reset/flow switch (`analysisStore.ts`). **Fix:** clear in `reset()` + `clearAnalysisState`.
- [x] **H2-fe** `presenceStore` module-level `setInterval` never cancelled (`presenceStore.ts:151`). **Fix:** early-return when `flowId == null` (no churn when disconnected/logged out).
- [x] **L8-fe** `flowStore.visibleTypes` not in `reset()`. **Fix:** reset to `new Set(ALL_TYPES)`.
- [x] **M1-fe** `Modal` renders inline despite `Portal` existing (zero importers). **Fix:** wrapped Modal in `<Portal>`.
- [x] **M2-fe** Modal a11y: no name when title omitted. **Fix:** added `ariaLabel` prop; `aria-label={title ?? ariaLabel}`.
- [x] **H3-fe** No retry on plain `request()`. **Fix:** bounded retry (2 attempts, jitter) for idempotent GETs / 5xx / network errors.
- [x] **L4-fe** `requestBlob` duplicates refresh logic. **Fix:** extracted shared `fetchWithRefresh` used by request + requestBlob + retry.
- [x] **M10** `tsconfig` legacy `moduleResolution: "Node"`. **Fix:** `"Bundler"` (both tsconfig + tsconfig.node).
- [x] **M11** Shallow code-splitting. **Fix:** lazy-loaded `AdminDashboard` (heavy routes were already lazy; admin kept out of non-admin bundle).
- [x] **M5-fe** settingsStore persist queue untestable. **Fix:** the three module-level queue vars (timer / resolveSuperseded / inflight) are now one `persistQueue` state object with a test-only `__resetPersistQueueForTest()` reset; `beforeEach`/`afterEach` clear it so cases don't leak. +3 Vitest cases (debounce coalesce, supersede-resolves-immediately, in-flight guard queues behind).
- [x] **L7-fe** `useChatStreamEngine` has no tests. **Fixed previously** (332-line test suite).

---

## Deferred / lower priority (not in active phases)

### Analyzer (Medium/Low)
- [x] M2 `patch.go:41-42` doc says "first occurrence" but code does `ReplaceAll` — update doc. **Fix:** doc updated to "all occurrences"; in-code comment was already correct.
- [x] M3 `ReportToMarkdown` panics on nil doc (`markdown.go:16`) — mirror SARIF nil-check. *(previously fixed)*
- [x] M4 `DiffFlows` O(m×n) memory + recursion risk — add size cap / Myers diff. **Fix:** `diffSubflow` now guards with `diffTooLarge(m,n)` (per-side block cap 5000, cell cap 4M); above it, `coarseBlockDiff` does an O(m+n) linear fallback (all-removed + all-added) with a fast path that keeps a large *unchanged* subflow exactly unchanged — no giant LCS matrix, no deep `backtrack` recursion. Below the cap the exact LCS is unchanged. +`TestDiffTooLarge`, `TestDiffSubflow_OverCap_CoarseFallback`, `TestDiffSubflow_OverCap_IdenticalIsUnchanged`.
- [x] M5 `blocksEqual` ignores property values (`diff.go:155`) — fold property hash. **Fix:** `blocksEqual` now also requires `maps.Equal(a.Properties, b.Properties)`, so a config-only edit (changed `Url:`/`Timeout:`/etc.) surfaces as remove-old + add-new instead of `ChangeNone`. +`TestBlocksEqual_DifferentPropertyValue`, `TestBlocksEqual_SameProperties` (order-independent), and end-to-end `TestDiffFlows_PropertyOnlyEdit`.
- [x] M6 SARIF `driver.version` never set. *(previously fixed)*
- [x] M7 SARIF per-rule level from first finding — track max severity. **Fix:** track `ruleMaxRank` per rule; bump `DefaultConfiguration.Level` when a higher-severity finding arrives.
- [x] M8 `report.Groups` order non-deterministic (`dedup.go:64`) — sort. **Fix:** `slices.SortFunc` by BlockID then Primary.Title; +25-iter determinism regression test.
- [x] M9 `detectCycles` malformed cycle reports — Tarjan SCC. **Fix:** swapped the DFS-with-colors reconstruction for Tarjan's SCC algorithm. Each cyclic SCC is now reported as its node set in canonical (sorted) order — no more duplicate leading element — and the list is deterministic across runs. A singleton SCC counts as a cycle only on a self-loop. +`TestDetectCycles_CanonicalOrder` (25-iter determinism), `TestDetectCycles_SelfLoop`, and the existing reports-cycle test now also asserts no duplicate node within a cycle.
- [x] M10 `progress.go` duplicates parse pipeline — unify. **Fix:** `Parser` gains a `WithProgress` option; `Parse()` runs the single tokenize→wrap→state-loop→finalize pipeline and emits percent callbacks only when a callback is wired (the no-callback hot path stays branch-free). `ParseTextWithProgress` now delegates instead of duplicating. This also fixed a latent bug: the progress copy was missing the canonical path's EOF unclosed-block flush, so an abruptly-terminated LOOP silently dropped its ParseError under the progress path. +`TestParseTextWithProgress_UnclosedBlock_MatchesParseText`.
- [~] M11 `safeCheck` swallows panics silently — add `RulesSkipped` surfacing. **Fix:** `AnalysisStats.RulesSkipped int` added; `safeCheck` returns `(findings, skipped)`; counter summed in `runAnalysisCore` and stamped on the report. +`TestRunAnalysis_RulesSkippedSurfaced` proves a panicking rule doesn't abort the run AND is observable on the report.
- [x] L1 Reinvent `max` builtin (`progress.go:61`, `diff.go:161`) — use builtin. **Fix:** deleted both; Go 1.25 builtin.
- [x] L2 `fnvHasher.write` operates on runes not bytes. **Fix:** iterate bytes (`for i := 0; i < len(s); i++`); +`TestFnvHasher_ByteSemantics` cross-checks canonical FNV-1a of UTF-8 bytes.
- [x] L4 `ApplyPatch` silently clamps out-of-range ops. **Fix:** a genuinely out-of-range target line (insert outside `[1,len+1]`, wrap/remove/replace/append `StartLine` outside `[1,len]`) is now a no-op that bumps the `patchOutOfRangeOps` atomic counter (exposed via `PatchOutOfRangeOps()`) + emits a `slog.Warn`, so a fixer bug is observable instead of silently corrupting output. Legitimate boundary cases (insert-at-EOF, wrap/remove range spanning to the last line) are preserved and NOT counted; signature of `ApplyPatch` unchanged. +`TestApplyPatch_OutOfRangeOpsAreNoOpsAndCounted` (7 op kinds) and `TestApplyPatch_LegitimateBoundaryOpsAreNotCounted`.

### Backend API/Service (Medium/Low)
- [x] M `analysis.go:62` constructor panics on LRU error — return error. **Fix:** `NewAnalysisService` now returns `(*AnalysisService, error)`; fx fails boot cleanly on (impossible-in-practice) error; 5 callers updated.
- [x] M `handlers_auth.go:119` maps all CreateUser errors → 409 — check `ErrEmailExists`. **Fix:** deleted special-case; pass `StatusInternalServerError` and let `render.Error`'s auto-map (`ErrEmailExists → 409`, everything else → 500) do the right thing. +regression test for the generic-500 path.
- [x] M `handlers_triage.go:166` batch triage non-atomic — add batch method. **Fix:** `BatchSetFindingStatus(ctx, flowID, userID, items)` added to `TriageStore` interface; Postgres impl wraps a single RLS-scoped tx (reuses middleware tx if present); filesystem impl is single read-modify-write under the mutex; FakeBackend stages in a local map so injected failures leave no partial state. +`TestTriage_SetBatch_AtomicityGuarantee` and `TestFakeBackend_BatchSetFindingStatus_AtomicWhenInjectedFail`.
- [x] M `handlers_analysis.go:22` package-global webhook notifier — DI. **Fix:** deleted `var defaultWebhookNotifier`; added `webhook *service.WebhookNotifier` field on `AnalysisHandler`; `service.NewWebhookNotifier` registered as an fx provider in `internal/di/services.go`; constructor parameter wired through `helpers_test.go`.
- [x] M `chat.go:770` 290-line function — extract `prepareTurn`. *(previously fixed)*
- [x] M `handlers_library.go:129` pagination cap inconsistent — use `clampListLimit`. *(previously fixed)*
- [x] L `handlers_flow.go:303` swallows JSON decode error. **Fix:** surface as `400 invalid request body: %w`; +`TestHandleReimport_MalformedBodyReturns400`.
- [x] L `chat.go:671` logger uses map form instead of slog kv. *(fixed in chat.go; persists in `chat_context.go:232` and `ai/tokens.go:25` — see "New issues" below)*
- [x] L capitalized "Forbidden" error strings. **Fix:** Phase 3A drive-by — 6 sites lowercased across `handlers_export.go`, `handlers_org.go`, `handlers_chat.go`, `websocket/handler.go`.
- [x] L `events.go:274` SSE event write error unchecked. *(previously fixed)*

### Storage (Medium/Low)
- [x] M1 `SaveAuditEvents` no 65535-param cap guard. **Fix:** the single multi-row INSERT (`len(events)*9` bind params) overflowed Postgres' 65535-param wire limit at >~7281 events. Now chunked into ≤5000-row batches (`chunkAuditEvents`/`buildAuditInsert`) run inside one `BeginTx` so the write stays all-or-nothing. +pure unit tests (`TestChunkAuditEvents_RespectsParamCeiling`, `TestBuildAuditInsert_ParamCount`) and a Postgres integration test (`TestSaveAuditEvents_LargeBatch`, 8000 rows) behind the `DATABASE_URL` podman harness — verified the pre-fix single statement fails with "extended protocol limited to 65535 parameters".
- [x] M2 `SaveOrg`/`MutateOrg` delete-all-reinsert members. **Fix:** both call sites now share one `syncOrgMembers(ctx, tx, org, now)` helper that upserts present members (`ON CONFLICT (org_id,user_id) DO UPDATE SET role` — preserving `joined_at`) and deletes only removed members via `DELETE ... WHERE org_id=$1 AND NOT (user_id = ANY($2))`. No more churn/`joined_at` rewrites on a no-op mutation. +Postgres integration test `TestSaveOrg_MemberSyncPreservesJoinedAt` (B's join time survives an A→C membership change; role change still applies).
- [x] M4 `SaveFlowVersion` uploads blob inside `FOR UPDATE` lock. **Fix:** the content blob is now keyed on the version **row id** (`versionBlobKey(flowID, v.ID)`) instead of the version number, so it's knowable before the version is allocated — the upload moves **ahead of** the `SELECT … FOR UPDATE`, so the parent-flow lock is no longer held across network I/O. New `blob_key` column (migration v10) stores the key; read/prune fall back to the legacy derived key (`versionBlobKeyLegacy`) for pre-migration rows. On lock/version-alloc failure the pre-uploaded blob is reclaimed. +`TestSaveLoadFlowVersion_DBPath` and the updated Azurite e2e `TestE2E_A5_…` (probes the id-keyed blob); legacy fallback covered by `TestE2E_A2_LegacyKeyFallback`.
- [x] M5 `FlowSortBlocksDesc` ORDER BY casts JSONB — add expression index. **Fix:** migration v9 adds `flows_blockcount_updated_idx` on `((COALESCE((metadata->>'BlockCount')::int, 0)) DESC, updated_at DESC)`, matching the sort expression exactly so that mode no longer full-scans + in-memory-sorts. Cast is safe (BlockCount is always a JSON number or absent→NULL→0). +`TestListFlows_SortByBlocksDesc` (asserts order 50,5,1 and the index's existence).
- [x] M7 `LoadUsersByIDs` IN-list vs `ANY($1)` inconsistency. **Fix:** replaced the hand-built `IN ($1,$2,…)` placeholder list (and its `#nosec G202` + latent >65535-param cap) with `WHERE id = ANY($1)`, passing the deduped `[]string` directly (pgx array encoding — same pattern as `postgres_dashboard.go:91`). +`TestLoadUsersByIDs_LargeN` (200 ids, with a dup + a missing id) alongside the existing contract test.
- L1 `IsRefreshTokenValid` redundant given atomic verify-and-revoke.
- [x] L6 `SearchKnowledge` loads 500 chunks into Go — pgvector pushdown. **Fix:** migration v11 `pgvector_knowledge` installs the `vector` extension (best-effort — no-ops if the role can't), adds a dimensionless `embedding_vec vector` column, backfills it from the existing JSONB embedding array, and builds an HNSW cosine index. `SearchKnowledge` now `ORDER BY embedding_vec <=> $query LIMIT n` server-side when pgvector is detected (boot-time probe); mismatched-dimension chunks are excluded from the index at insert time so similarity stays well-defined. Dimension is a deploy-time contract (`PAD_EMBEDDING_DIM`, default 1536). The Go-side `rankKnowledgeChunks` ranker remains as the portability fallback (local mode / no extension / dim mismatch). +`FormatVector` unit tests, dispatch-predicate tests, a config-loader test, and a `DATABASE_URL`-gated integration test.

### Frontend (Medium/Low)
- [x] M3-fe `request()` param ordering — options-bag. **Fix:** `request`/`requestValidated`/`requestBlob` now take a `RequestOptions` bag (`{body, method, timeoutMs}`); body-less GETs read `request('/x', {method:'GET'})` instead of `request('/x', undefined, 'GET')`. All ~56 call sites + tests migrated; `tsc`/`eslint`/Vitest green.
- [x] M4-fe `listShares` returns `unknown[]` — type it. **Fix:** returns `ShareInfo[]` (new typed interface in `flow.ts`).
- [x] M6-fe `ChatInput` history `useMemo` stale after sending. **Fixed previously** (reactive `userMsgCount` dep).
- [x] M7-fe `CommandPalette` render-order side-effect counter. **Fix:** the flat index is now precomputed into `indexById` in a `useMemo` (decoupled from render); the render body reads `indexById.get(cmd.id)` instead of mutating a `let itemIndex` counter, so the render is pure regardless of any future grouping/sort divergence.
- [x] M8-fe `WebAdapter` file-dialog 5-min fallback timer. **Fix:** added `window.focus`-based dismissal to both web file dialogs — when the dialog closes without a selection (some browsers don't fire `oncancel` on Esc), the promise now resolves null promptly instead of leaving the caller dead for 5 minutes. A `gotChange` flag set synchronously in `onchange` prevents a false cancel while the async folder-read is in flight; the long fallback stays as a backstop.
- [x] M9-fe Icon-only buttons rely on `title` only. **Fix:** added `aria-label` to the remaining icon-only buttons (`HistoryTab` compare/restore, `OrganizationsPanel` delete-org/remove-member); text+icon buttons (StatusBar admin/profile) already get an accessible name from their visible label.
- [x] L1-fe `resetAllStores` swallows errors silently. **Fix:** the per-handler catch now `logger.error`s the failure (with index) instead of `() => {}`, so a logout-time error (e.g. presence failing to re-persist its sync queue) is visible. Isolation preserved. +test (throwing handler doesn't abort others + is logged).
- [x] L2-fe `logger.error` prod / `logger.warn` dev-only asymmetry. **Not a bug (verified):** the asymmetry is intentional — `warn` is dev-noise suppression (keeps prod logs clean), `error` always surfaces. Non-issue; left as-is.
- [x] L5-fe `useSettingsPersistence` unused dep. **Not a bug (verified):** both effect deps are meaningful — `isAuthenticated` gates the backend load (re-triggers on login, avoids a doomed unauthenticated fetch; the guard is documented in-code), and `updateLayout` is the persisted writer. No dead dependency.
- [x] L6-fe `getMessages` returns live array reference. **Fix:** `chatStore.getMessages` returns a defensive copy (`[...msgs]`) so an accidental in-place mutation by a caller can't corrupt the store's internal array or bypass reactivity. `EMPTY_ARRAY` (already immutable) is returned as-is for missing threads.
- [x] L9-fe `TauriAdapter` retry loop unbounded. **Fix:** `getBackendConfig`'s sidecar-ready retry path now has a 60s hard deadline (`SIDEKICK_READY_DEADLINE_MS`) — a sidecar that never starts (crash/misconfig) now rejects with a clear error instead of retrying forever on a blank screen (and leaking the `backend-ready` listener). +tests (rejects after deadline; resolves when a retry invoke succeeds).

### Security/Infra (Low)
- [x] L1 keystore bypasses RLS tx. **Clarified (not a real gap):** `provider_keys` has no RLS policies by design (auth-infra table like refresh_tokens/api_tokens); every query carries `WHERE user_id = $1` and AES-GCM AAD binds each ciphertext to its row. Added guard comments on the table DDL + the `EncryptedKeyStore` type doc so a future RLS addition remembers to switch to BeginRLS + give the retention purge a principal.
- [x] L4 account-export temp file mode not explicit. **Fix:** explicit `os.Chmod(tmp.Name(), 0o600)` after `os.CreateTemp` (which already opens 0600) so the owner-only mode is visible at the call site and survives a future API swap.
- [x] L6 weak-secret blocklist only 7 entries. **Fix:** expanded `knownWeakSecrets` with common ≥32-char documented placeholders (tutorials/READMEs/jwt.io/boilerplate) that slip past the length floor, plus a new `isLowEntropy` check catching long-but-pathological secrets (all-identical chars, 2–4-byte tiles repeated) — no realistic false-positive risk. +`TestIsWeakSecret_BlocklistAndEntropy`.
- L8 Tauri CSP broad localhost connect-src.
- I4 No SBOM generation in CI.
- I5 Dockerfile HEALTHCHECK redundant under ACA.

---

## Phase 3A — Security + correctness + supply chain (2026-07-19)

- [x] **H1** `SaveKnowledgeChunks` bypassed RLS (`postgres_orgs.go`). **Fix:** mirrors the `SaveFlowVersion` RLS pattern — `BeginRLS(ctx, userID)` when no middleware tx is on ctx, otherwise reuse it. `KnowledgeStore.SaveKnowledgeChunks` now takes a `userID` parameter; threaded from RAG service via `auth.ClaimsFromContext`. +Postgres RLS regression test `TestPostgres_RLS_SaveKnowledgeChunks_EnforcesOrgMembership`.
- [x] **H2** `migrateSettings` unconditionally overwrote dst (re-run rolled back admin tuning). **Fix:** skip-if-present — dst with rule overrides or recent files is treated as authoritative.
- [x] **H3** `migrateOneConversation` unconditionally overwrote dst (re-run clobbered post-migrate chat). **Fix:** skip-if-present via `LoadConversation`; `Result.ConversationsSkipped` counter added. Bonus: cancelled walk no longer silently swallowed.
- [x] **H4** `startServer` called `os.Exit(1)` on listener-bind / session-secret failure, bypassing the fx error path. **Fix:** returns `fmt.Errorf(...)` so fx surfaces boot failures cleanly.
- [x] **H5** WebSocket connections not drained on SIGTERM (`http.Server.Shutdown` doesn't close hijacked sockets; `Hub.Close` only stopped the Redis backplane). **Fix:** new `Hub.Shutdown(ctx)` snapshots all clients, sends each a `CloseGoingAway` control frame + closes the conn, waits (bounded by ctx) for every client's pumps to exit via `Client.done`. Wired into `startServer` OnStop BEFORE `server.Shutdown` with a 5s budget. +`TestHub_Shutdown_DrainsAllClients` and `TestHub_Shutdown_Idempotent`.
- [x] **H6** `bakicli` binary re-tracked in git (H10 regression from commit `53e10e3`). **Fix:** `git rm --cached bakicli`; new `no-tracked-binaries` CI job fails the build if it ever recurs.
- [x] **H7** `action.yml` upload-sarif step ran regardless of `inputs.format` → JSON-as-SARIF silent misclassification. **Fix:** gate on `inputs.format == 'sarif'` with a warning step explaining the skip when the combination is invalid.
- [x] **H8** Release artifacts unsigned, no checksums. **Fix:** `release.yml` now emits a `<asset>.sha256` sidecar next to each archive; both uploaded to the GitHub Release. (Cosign signing deferred — needs key management.)
- [x] **H9** No GitHub Action was SHA-pinned — supply-chain hole (cf. 2025 `tj-actions/changed-files` compromise). **Fix:** every `uses:` in all 6 workflow files pinned to `@<40-char-sha>` with `# <tag>` comment. Dependabot's `github-actions` updater keeps both in sync. Drive-by fix: `azure/arm-deploy-action@v2` was a 404 (real repo is `Azure/arm-deploy`) — corrected.
- [x] **Drive-by** Capitalized "Forbidden" / "Internal Server Error" error strings (Go convention: lowercase). 6 sites in `handlers_export.go`, `handlers_org.go`, `handlers_chat.go`, `websocket/handler.go`.
- [x] **Drive-by** `RulesSkipped` (plumbed by Tier 2A M11) was not consumed anywhere. **Fix:** `pad_rules_skipped_total` Prometheus counter incremented in `AnalyzeFlow` when `report.Stats.RulesSkipped > 0`; `bakicli` text output prints a stderr warning so CI operators see findings may be incomplete.

---

## Phase 3B — Phase 3A regressions + cheap security wins (2026-07-19)

- [x] **U1+U2** Phase 3A SHA-pin bug: `action.yml:127` pinned `github/codeql-action/upload-sarif@b7351df…c304a1758ef9895495fa` — 43 hex chars (git SHAs are 40). Re-resolved via `gh api repos/github/codeql-action/git/refs/tags/v3` → real commit `b7351df727350dca84cb9d725d57dcf5bc82ba26`. Verified all 50 `uses:` pins across the 6 workflow files are exactly 40 hex chars.
- [x] **U3** Tauri capability file was wide open: `src-tauri/capabilities/default.json` granted `shell:allow-spawn / shell:allow-kill / shell:allow-execute / shell:default` + `core:default` to the webview despite the Rust host owning all process spawning. **Fix:** rewrote to minimum explicit permissions (`core:event:default`, `core:webview:default`, the 5 explicit `core:window:allow-*`, `dialog:allow-open/save`, `shell:allow-open` for URL opening only, `log:default`). XSS in PAD-rendered content can no longer spawn processes.
- [x] **U4** Dockerfile + docker-compose HEALTHCHECK hit `/healthz` (always 200) → silent on DB outage. **Fix:** both now hit `/readyz` (DB + blob + Redis reachability).
- [x] **U5** Phase 3A removed `hub.Close()` OnStop as redundant with `Hub.Shutdown`. Restored as a safety net — `Hub.Close` is idempotent so a double-call is a no-op; if `startServer`'s OnStop never fires (fx failure path), the Redis backplane subscriber still gets released.
- [x] **H11** AcceptInvite TOCTOU: `MarkOrgInviteAccepted` ran `UPDATE org_invites SET accepted_at=$1 WHERE id=$2` with no `AND accepted_at IS NULL` guard. **Fix:** added the guard + `RETURNING id`; new sentinel `interfaces.ErrOrgInviteAlreadyAccepted` maps to `collaboration.ErrInviteAlreadyAccepted`. Two concurrent AcceptInvite calls now have exactly one winner.
- [x] **H12** AcceptInvite trusted JWT `claims.Email` without verifying `EmailVerified`. **Fix:** handler now `LoadUserByID` and reads `u.EmailVerified`; `AcceptInvite` takes `emailVerified bool` and rejects with new `ErrEmailNotVerified` when false. A shadow local account with `victim@example.com` (never verified) can no longer accept invites destined to the victim. +2 regression tests.
- [x] **H13** SSO OIDC provider cached for process lifetime — IdP key rotation broke every login until restart. **Fix:** 15-min TTL via `providerFetchedAt`; `invalidateProvider` called from `Exchange` on `idToken.Verify` failure forces synchronous rediscovery. +2 tests.
- [x] **H14** SSO external calls used caller ctx verbatim — hung IdP pinned handler goroutines indefinitely. **Fix:** discovery, code exchange, and id_token verify all wrapped in `context.WithTimeout(ctx, 15*time.Second)`. +1 test pointing at a TCP listener that accepts but never responds.
- [x] **H15** Scrubber missing major secret formats: AWS (AKIA/ASIA), Google (AIza), Slack (xox[abprs]-), JWT (eyJ…), PEM private key blocks. **Fix:** 5 new regexes in `secretRegexes` + matching 5 `viablePrefixRegexes` entries (preserves the streaming-scrubber sync invariant). +6 test vectors.
- [x] **H20** No metric for background-loop liveness — scanner, padcloud ingester, retention purge could silently hang with zero signal. **Fix:** `pad_background_loop_tick_total{loop}` counter + `pad_background_loop_last_tick_timestamp_seconds{loop}` gauge via new `metrics.RecordBackgroundLoopTick(name)`; called from the 3 periodic loops (worker pools like blob_cleaner/audit_pool are not periodic and are documented as such). +1 test.
- [x] **H21** `/readyz` did not check Redis when configured — a Redis outage silently degraded multi-replica correctness (rate limiter, hub presence, chat-resume all fail open). **Fix:** new `RedisPinger` interface on `SystemHandler`; readiness pings Redis when non-nil, falls through to the existing 3-consecutive-failure threshold. Adapter `ProvideRedisPinger` in `internal/di/api.go` wraps `*redis.Client` (nil in single-replica mode). +2 tests.
- [x] **H19** Secret rotation impact was undocumented — operators didn't know `PAD_AUTH_SECRET` rotation force-logs-out every user, `PAD_ENCRYPTION_KEY` rotation bricks provider keys + PAD-cloud tokens. **Fix:** new §7a "Secret rotation impact" table + §7b "Zero-downtime rotation (forward path)" in `docs/DR_RUNBOOK.md`.

---

## New issues (discovered in 2026-07-19 deep-dive, not yet fixed)

- [x] **AGENTS.md doc drift** — already accurate (Project Structure now reads "41 rules + 17 auto-fixers").
- **Logger antipattern persists** in `internal/service/chat_context.go:232` and `internal/ai/tokens.go:25` — both pass `map[string]interface{}{"error": err}` to `slog` instead of variadic kv. **Fixed** in both files (now `logger.Error("...", "error", err)`).
- [x] **`AnalyticsDashboard.tsx` (436 lines)** — was a god-component candidate; **decomposed** into a thin shell wiring two hooks (`useDashboardStats`, `useBatchAnalysis`) to five presentational tiles (`StatCard`, `SeverityChips`, `RuleBarChart`, `TopProblemFlows`, `BatchResultsTable`). +`useDashboardStats.test.tsx` covering the reqId race guard + background-refresh-failure-toasts-instead-of-wiping contract.
- [x] **`presenceStore` module-level `setInterval`** (`stores/presenceStore.ts:153`) — H2-fe was "fixed" with an early-return guard but the 60s timer itself is never cancelled, so the closure lives for the page's lifetime even when logged out. **Fix:** the sweep is now lifecycle-managed — `startPresenceSweep()` (called in `connectToFlow`) creates the interval, `stopPresenceSweep()` (called in `disconnect`, which already runs on logout via `registerStoreReset`) clears it. The timer exists only while a flow is connected. +3 fake-timer Vitest cases (sweeps while connected, clears on disconnect, no double-start).
- [x] **No root-level `ErrorBoundary`** in `App.tsx`/`main.tsx` — already added: `<ErrorBoundary>` wraps the provider layer in `App.tsx`.

---

## Phase 5 — pgvector pushdown + audit durability + frontend reliability (2026-08-01)

A deep-dive-driven pass closing the highest-impact open items (the verified-open subset of the IMPROVEMENTS/PRODUCTION_READINESS trackers).

- [x] **A** `SearchKnowledge` pgvector pushdown (was L6). **Fix:** migration v11 + server-side cosine ranking + `PAD_EMBEDDING_DIM` + Go-side fallback. *(detailed note at the L6 line above)*
- [x] **B** `AnalyticsDashboard.tsx` decomposition. **Fix:** 436-line god-component → thin shell + 2 hooks + 5 tiles + a hook test for the race/background-refresh contract. *(detailed note in "New issues" above)*
- [x] **C** `progress.go` pipeline unification (was M10) + latent EOF unclosed-block bug fix. *(detailed note at the M10 line above)*
- [x] **E** Audit durable sink (PROD item 5). **Fix:** on-disk spill queue (`auditSpillStore`) + a 500 ms-tick reaper (`auditSpillReaper`→`replaySpilled`) that drains overflow back into the pool while it has headroom (< half full, so fresh events aren't starved). Size-capped (default 10 MB; newest-at-cap degrades to the existing log fallback, metered via `pad_audit_spill_dropped_total`). New metrics `pad_audit_spilled_total` / `pad_audit_spill_replayed_total` / `pad_audit_spill_dropped_total`. `PAD_AUDIT_SPILL_DIR` (empty=temp dir, "off"=disabled). +5 tests (FIFO, size-cap drop, reSpill no double-count, replay-into-pool, yields-at-half-full).
- [x] **F** Frontend type-safety sweep: `request()`/`requestValidated`/`requestBlob` → `RequestOptions` bag (+56 call sites migrated); `listShares` → `ShareInfo[]`; `settingsStore` persist queue testable (`__resetPersistQueueForTest`); icon-only button `aria-label`s.
- [x] **D** Reconciled stale tracking docs (this section + the inline `[x]` updates).

**Still genuinely open** after this pass: `detectCycles` cosmetic format (M9 — **done**, Tarjan SCC), filesystem silent-success stubs (H8 — interface split, deferred as large/risky), and the lower-priority items (~~M7-fe CommandPalette render-order~~ **done** (precomputed `indexById`), ~~L5-fe useSettingsPersistence unused dep~~ **verified non-issue**, ~~L6-fe getMessages live array ref~~ **done** (defensive copy); Security/Infra L8 Tauri CSP / I4 SBOM / I5 redundant HEALTHCHECK; PROD items 19 container hardening / 20b CSP unsafe-inline; ~~20c RuntimeConfig retry/circuit-breaker unused~~ **done** (wired in `factory.go`)).

---

## Verification gates (run per phase)

```bash
go test ./... && \
go test -race ./core/analyzer/... ./internal/api/... ./internal/service/... ./internal/storage/... && \
golangci-lint run && \
cd frontend && npx tsc --noEmit && npm test && npm run lint
```
