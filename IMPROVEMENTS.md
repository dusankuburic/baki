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

- [x] **H10** `bakicli` (tracked binary, 5.2 MB) bypasses `.gitignore`. **Fix:** `git rm --cached bakicli`; add `/bakicli` to `.gitignore`.
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
- [ ] **M3-RLS** `BeginTx` paths bypass RLS undocumented (`postgres_orgs.go`, `SaveFlowVersion`, `SaveKnowledgeChunks`). **Fix:** document or route via `BeginRLS`. *(deferred)*
- [ ] **M9** Migration system has no checksum. **Fix:** sha256 column in `schema_migrations`; fail boot on mismatch. *(deferred)*

---

## Phase 4 — Frontend reliability

- [ ] **H9** `FindingCard.tsx` (459 lines, 6 concerns, zero tests). **Deferred** — large refactor; tracked separately. *(deferred to avoid churn risk)*
- [x] **H1-fe** `focusedFindingKey` not cleared on reset/flow switch (`analysisStore.ts`). **Fix:** clear in `reset()` + `clearAnalysisState`.
- [x] **H2-fe** `presenceStore` module-level `setInterval` never cancelled (`presenceStore.ts:151`). **Fix:** early-return when `flowId == null` (no churn when disconnected/logged out).
- [x] **L8-fe** `flowStore.visibleTypes` not in `reset()`. **Fix:** reset to `new Set(ALL_TYPES)`.
- [x] **M1-fe** `Modal` renders inline despite `Portal` existing (zero importers). **Fix:** wrapped Modal in `<Portal>`.
- [x] **M2-fe** Modal a11y: no name when title omitted. **Fix:** added `ariaLabel` prop; `aria-label={title ?? ariaLabel}`.
- [x] **H3-fe** No retry on plain `request()`. **Fix:** bounded retry (2 attempts, jitter) for idempotent GETs / 5xx / network errors.
- [x] **L4-fe** `requestBlob` duplicates refresh logic. **Fix:** extracted shared `fetchWithRefresh` used by request + requestBlob + retry.
- [x] **M10** `tsconfig` legacy `moduleResolution: "Node"`. **Fix:** `"Bundler"` (both tsconfig + tsconfig.node).
- [x] **M11** Shallow code-splitting. **Fix:** lazy-loaded `AdminDashboard` (heavy routes were already lazy; admin kept out of non-admin bundle).
- [ ] **M5-fe** settingsStore persist queue untestable. **Deferred.**
- [ ] **L7-fe** `useChatStreamEngine` has no tests. **Deferred.**

---

## Deferred / lower priority (not in active phases)

### Analyzer (Medium/Low)
- M2 `patch.go:41-42` doc says "first occurrence" but code does `ReplaceAll` — update doc.
- M3 `ReportToMarkdown` panics on nil doc (`markdown.go:16`) — mirror SARIF nil-check.
- M4 `DiffFlows` O(m×n) memory + recursion risk — add size cap / Myers diff.
- M5 `blocksEqual` ignores property values (`diff.go:155`) — fold property hash.
- M6 SARIF `driver.version` never set.
- M7 SARIF per-rule level from first finding — track max severity.
- M8 `report.Groups` order non-deterministic (`dedup.go:64`) — sort.
- M9 `detectCycles` malformed cycle reports — Tarjan SCC.
- M10 `progress.go` duplicates parse pipeline — unify.
- M11 `safeCheck` swallows panics silently — add `RulesSkipped` surfacing.
- L1 Reinvent `max` builtin (`progress.go:61`, `diff.go:161`) — use builtin.
- L2 `fnvHasher.write` operates on runes not bytes.
- L4 `ApplyPatch` silently clamps out-of-range ops.

### Backend API/Service (Medium/Low)
- M `analysis.go:62` constructor panics on LRU error — return error.
- M `handlers_auth.go:119` maps all CreateUser errors → 409 — check `ErrEmailExists`.
- M `handlers_triage.go:166` batch triage non-atomic — add batch method.
- M `handlers_analysis.go:22` package-global webhook notifier — DI.
- M `chat.go:770` 290-line function — extract `prepareTurn`.
- M `handlers_library.go:129` pagination cap inconsistent — use `clampListLimit`.
- L `handlers_flow.go:303` swallows JSON decode error.
- L `chat.go:671` logger uses map form instead of slog kv.
- L capitalized "Forbidden" error strings.
- L `events.go:274` SSE event write error unchecked.

### Storage (Medium/Low)
- M1 `SaveAuditEvents` no 65535-param cap guard.
- M2 `SaveOrg`/`MutateOrg` delete-all-reinsert members.
- M4 `SaveFlowVersion` uploads blob inside `FOR UPDATE` lock.
- M5 `FlowSortBlocksDesc` ORDER BY casts JSONB — add expression index.
- M7 `LoadUsersByIDs` IN-list vs `ANY($1)` inconsistency.
- L1 `IsRefreshTokenValid` redundant given atomic verify-and-revoke.
- L6 `SearchKnowledge` loads 500 chunks into Go — pgvector TODO.

### Frontend (Medium/Low)
- M3-fe `request()` param ordering — options-bag.
- M4-fe `listShares` returns `unknown[]` — type it.
- M6-fe `ChatInput` history `useMemo` stale after sending.
- M7-fe `CommandPalette` render-order side-effect counter.
- M8-fe `WebAdapter` file-dialog 5-min fallback timer.
- M9-fe Icon-only buttons rely on `title` only.
- L1-fe `resetAllStores` swallows errors silently.
- L2-fe `logger.error` prod / `logger.warn` dev-only asymmetry.
- L5-fe `useSettingsPersistence` unused dep.
- L6-fe `getMessages` returns live array reference.
- L9-fe `TauriAdapter` retry loop unbounded.

### Security/Infra (Low)
- L1 keystore bypasses RLS tx.
- L4 account-export temp file mode not explicit.
- L6 weak-secret blocklist only 7 entries.
- L8 Tauri CSP broad localhost connect-src.
- I4 No SBOM generation in CI.
- I5 Dockerfile HEALTHCHECK redundant under ACA.

---

## Verification gates (run per phase)

```bash
go test ./... && \
go test -race ./core/analyzer/... ./internal/api/... ./internal/service/... ./internal/storage/... && \
golangci-lint run && \
cd frontend && npx tsc --noEmit && npm test && npm run lint
```
