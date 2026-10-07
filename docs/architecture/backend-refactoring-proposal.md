# Backend refactoring proposal: making Baki extensible (revision 3, two review passes applied)

## Status (2026-10-07)

**Fix first:** R1–R6 and S1–S3 are done. R7 and R8 come with Phase 4c.

**Phase 2a: done** (2026-10-07, uncommitted).

*Delivered:*
- **Services own the storage work.** Handlers make no direct storage calls any more: the `backend.X` ratchet is 0
  (from 85), and a second ratchet (`internal/archtest/storehandles_test.go`, go/types) pins the remaining
  infrastructure store handles per file (PAT auth, the Idempotency-Key store, the migration lock, the Redis health
  probe). New services take narrow role interfaces: `TriageService`, `CommentService`, `ShareTokenService`,
  `PolicyService`, `GovernanceAlertService`, `OrgRuleService`, `ChannelService`, `UserDirectory`,
  `CollaboratorService`, `SessionService`, `IdentityService`, `APITokenService`, `UserAdminService`,
  `HealthService`, and `AccountLifecycleService`, which runs erasure as prepare → delete → finalize in the old order.
- **`internal/audit`** (ADR 0008): an fx-managed `*audit.Recorder` replaces the package-global pool in `api`;
  `Record(ctx, audit.Entry{…})` takes the user, actor, token, IP and user agent from the context. Every handler
  records through its injected recorder (83 sites; `logAudit` and the global default are gone) and passes the
  organisation where it is already in hand. Migration v33 adds `org_id`, `actor_kind`, `token_id`, `actor_job` and
  `user_agent`; a boot-time job backfills `org_id` under an advisory lock, in statements of at most 1,000 rows.
  The existing columns are byte-identical to before (tested across actors, proxies and address forms).
- **DI:** `AIBudget` and `FlowScopeResolver` are extracted, the other setters became constructor parameters, and
  `internal/di/setters_test.go` pins the remaining ones, each with its reason (`docs/architecture/di-wiring.md`).
- `apperr` moved to `internal/platform/apperr`, so services may return typed errors.

*Found in review and fixed:* the flow phase of the backfill could exceed the pool's 30 s statement timeout on a
large table and then fail at the same page on every boot; erasure left the user agent on the user's pre-auth rows.

*Open:* audit rows of personal flows and of flows deleted before v33 keep a NULL organisation (by design);
`FlowService.SetRuleProfileResolver` stays a setter (55 test call sites); the four event-hook setters are replaced
by Phase 2b's event bus.

**Phase 1b: done** (2026-10-07, uncommitted).

*Delivered:*
- **Typed endpoints** (`internal/api/endpoint.go`, `page.go`, `apperr`): `JSON`/`Query` adapters run the strict
  decode, URL parameters, page bounds, `Validate()`, `AuthorizeFlow` and the function in a fixed order, and refuse a
  misdeclared request when the handler is built. `render` honours `*apperr.Error` (byte-identical envelope for every
  other error). `Page[T]` and `PageRequest`; conventions in `docs/architecture/api-conventions.md`. No existing
  handler was migrated; `TestArch_RoutesUseTheTypedAdapter` pins the hand-written routes per file, so new routes use
  the adapter.
- **`Idempotency-Key`** for 14 create routes (`route.Idempotent()`, ADR 0007): migration 0032 (the first `.sql`
  step, `expand`), the claim inside the request transaction under an advisory lock, a concurrent duplicate answers
  409 `IDEMPOTENCY_IN_PROGRESS`, another body under the same key 422, bodies sealed at rest, 24 h retention purge.
  Postgres store, fake and a contract suite. CORS allows the header and exposes `Idempotent-Replayed`.
- **Frontend:** tolerant enums (`tolerantEnum`, `Tolerant<T>`) for provider, model, Azure, alert, channel and
  triage values; roles and finding severity stay closed. `X-Baki-Version` on every response and a reload banner,
  offered once per server build; the image stamps the version into `index.html`, not a chunk.

*Deferred:* the frontend does not send `Idempotency-Key` yet (one key per user action, and `fetchWithRetry` could
then retry those POSTs); cursor/sort helpers until the first endpoint needs them.

**Phase 1a: done** (2026-10-07, uncommitted; the rollout flag defaults to `shadow`).

*Delivered:*
- **Modules and declared route policy** (ADR 0006): every handler is an `api.Module` contributed through the fx
  `routes` group and declares its routes and policy in `Mount(rb)`. The `Handlers` struct, `ProvideHandlers` and the
  central route list are gone. The request is resolved once in the outer layer; each endpoint's guard checks chi's
  match against it (500 on a mismatch in declarative mode); a route not registered through the builder fails the
  boot. `PAD_ROUTE_POLICY=legacy|shadow|declarative` (Bicep `routePolicy`, compose), shadow counting mismatches in
  `pad_route_policy_mismatch_total`. The metrics label is the matched pattern (dashboards: `:id` becomes `{param}`).
- **Request info and actor:** `reqinfo` and `auth.Actor` (user, token, system); the access log's `user_id` is
  filled and app logs carry the request ID (`logger.FromContext`).
- **Authorization through `AuthzService.Can`**, token scopes included (closes the S2 class): flows (27 sites, route
  middleware for the path-parameter routes), orgs, `/api/admin` as one group middleware, the WebSocket flow checker.
  Four sites stay on their role checks because `Can`'s scope rule would tighten what a token may do today (rule
  enable/config, GET snapshots, GET invites, chat save/clear).
- **Mode gating:** `route.Needs(capability.ServerStore)` on 52 routes replaces their handler 503s.
- **CI:** the previous release boots against the new schema (`previous-release-boot`).
- **Security fixes found while building:** the SSO flow cookie is signed and verified (an attacker could link their
  IdP identity to a victim's account); `/swagger/*` accepted any `Authorization` header.

*Deviations from the plan, and why:*
- Org admin writes that also run on the desktop's in-memory org store (rename, delete, invite revoke, role change)
  use `AuthorizeOrg`, which reads the org first, because `Can` allows every org action without storage; one roster
  read in cloud.
- On the desktop, a few routes now answer 503 before validating their body or session (account delete/export,
  admin role PUT, collaborator add/update), because `Needs` is decided before the handler.

*Open:* flip to `declarative` after a soak with zero shadow mismatches, then delete the legacy tables one release
later. Tightening the four role-check sites to `Can` is a product decision (it changes what existing tokens may do).
`securityAuthz` builds an `AuthzService` per call where the handlers have no injected one.

**Phase 0b: done** (uncommitted).

*Delivered:*
- **`internal/platform/txhook`:** a post-commit registry that is pending, committed or aborted.
  - Aborted hooks never run, including ones registered after the rollback.
  - Late registrations after a commit run at once.
  - A panicking hook is contained.
  - All 12 call sites moved to it, so API and service code no longer import the storage package for it.
- **`withTx(ctx, func(txCtx, tx))`:** when it opens its own transaction, `txCtx` carries a registry committed
  after that transaction commits. The three hand-rolled copies (SaveFlow, SaveFlowVersion, the Azure owner lock)
  now use it.
- **`storagedb.RLSHookRunner`:** each hook gets a fresh lazy RLS scope for the same user.
- **R5:** the RLS middleware commits before the first response byte (`txResponseWriter`). A failed commit becomes
  a 503 with the entry headers restored, and a panic after the commit still runs the post-commit work
  (ADR 0004).
- **`lifecycle.Bounded`:**
  - in-flight and duration metrics;
  - panics reported at ERROR through the error sink;
  - every runner registered with a group that `lifecycle.DrainAll` drains at shutdown. It waits for all runners
    to go idle before closing any, so a chained hand-off is not dropped.
  - `Router.DrainBackground` is gone.
  - New runners: the invite email (R4), admin jobs (`runDetached`, now 429 when saturated) and AI usage writes.
- **S3:** channel URLs and secrets are encrypted at rest with the keystore key, plus a one-shot backfill per boot
  under a cross-replica lock (ADR 0005).
- **Ratchets lowered:** API files importing the storage package, 4 → 1; transaction sites outside `withTx`, −3.

*Deviations from the plan, and why:*
- **The RLS context key stays** in the storage package. Hook contexts come from a `txhook.Runner` that the
  transaction's owner supplies, which needs no move.
- **`txhook.AsSystem` is deferred** until a consumer needs a system scope (Phase 2b).

*Found while building:*
- Two audit EXPLAIN tests failed on every fresh database, CI's included. They passed only after earlier runs had
  scrambled the shared table. Now each gets its own database and a seed in random physical order.
- A stray trailing space in `postgres_lock.go` broke gofmt; removed.

**Phase 0a: done** (2026-10-06, uncommitted).

*Delivered:*
- **Migrations:**
  - compat markers recorded in `schema_migrations`;
  - a boot guard that accepts expand-only newer schemas (ADR 0002);
  - an embedded `.sql` loader for v32+ that strips the BOM and CR, with `.gitattributes`;
  - a per-step `lock_timeout` with retry;
  - a checksum golden.
- **Deploy:** a startup probe plus a longer smoke-test window (ADR 0003), and the schema-aware rollback guard.
- **Test harness:** `internal/testutil/pgtest`, isolated databases cloned from a migrated template; CI grants
  `CREATEDB` and creates `baki_template`.
- **Safety nets:**
  - `TestAppGraph` (`fx.ValidateApp` over main's graph);
  - the route-policy golden (1,249 requests);
  - the authz matrix golden;
  - the storage contract run against FakeBackend (9 known divergences listed);
  - architecture ratchets in `internal/archtest`.
- **Benchmark:** `BenchmarkBuildHandler`, plus a nightly same-runner benchstat job.
- **Conventions:** `internal/capability` with the profile matrix, `docs/architecture/*`, `docs/adr/*`, and a root
  `CLAUDE.md`.

*Deviations from the plan, and why:*
- **Boot ordering.** A startup probe instead of binding the listener before migrating. The goal (liveness never
  kills a long migration) is reached with configuration, not a restructured boot; see ADR 0003.
- **Test routers from the real fx modules** moved to Phase 1a, because it needs the `api.Module` seam. Phase 0a
  validates main's full graph once: deployment modes change values, not the graph's types.
- **The "previous release boots on the new schema" CI job** waits until a release containing the compat guard
  exists, since older binaries refuse every newer schema anyway. `TestMigrate_AcceptsNewerExpandOnlySchema`
  covers the guard.
- **Ratchets as a Go test** instead of golangci `depguard`/`forbidigo`: pinned per-file counts that must also
  shrink are not expressible in golangci.

*Found while building the nets:*
- A knowledge-documents plan test passed 2 of 3 query arguments. It failed whenever `DATABASE_URL` was set;
  now fixed.
- The benchmark's save case timed `409` responses until it sent the stored version.
- Recorded for Phase 1a to keep or fix deliberately:
  - `GET /x%2F..%2Fshared` is charged to the general rate-limit bucket while the `/shared` fallback serves it;
  - read-style POST endpoints need the `write` scope for API tokens.

## Context

Baki's backend is about 100k lines of Go: the root module (`internal/*`, `main.go`) plus `./core`, which is the separate `pad-core` module (parser, analyzer, export).

The base is solid:
- fx dependency injection
- per-domain services
- storage role interfaces
- RLS-aware `withTx`
- advisory-lock-guarded jobs
- Redis backplanes for rate limiting, WebSocket and chat resume
- self-registering analyzer rules
- size-adaptive compression and byte-weighted caches

The problem is the cost of change. One feature touches **6–10 central files** (commits `9cc25c0`, `7218f55`, `b613b02`). Correctness depends on many hand-synced lists, and some have already drifted into bugs.

**Goal:** a feature is one self-contained module. Cross-cutting policy is declared once, next to the feature:
- auth and scopes
- rate limits, RLS mode, timeouts and metrics labels
- audit
- cache invalidation and realtime
- data lifecycle
- migrations

Invariants are enforced by tests and linters, not comments.

**User decisions:**
- All four extension areas matter: product features, AI providers and tools, analyzer rules and exports, and scale-out.
- **Desktop/local mode stays as it is** (nil storage backend, cloud features off), but becomes explicit through a mode capability matrix.
- The filesystem backend is frozen as a read-only legacy migration source.
- The test fake stays and gets contract-tested.

**Constraints:**
- URLs and JSON stay unchanged, except for the flagged frontend work.
- Each phase ships on its own.
- Never commit; the user commits or tags at phase boundaries, and those are the rollback points.

**Effort for one developer:**

| Tier | Contents | Time | What it unlocks |
|---|---|---|---|
| Tier 1 | Fix-first + Phases 0–2 | about 5 months | Cheap new product features |
| Tier 2 | Phases 3, 4a, 4c, 5a | about 3 months | |
| Tier 3, on demand | 4b when a new provider is scheduled; 5b when more than one replica is needed | about 2.5 months | |

### Review log

- **Pass 1 (revision 1 → 2):**
  - Desktop mode has a nil backend (filesystem is legacy only).
  - Jobs already hold advisory-lock leases; the real scale-out blocker is the in-process SSE hub plus per-process chat state.
  - Per-route `r.With` would reorder middleware.
  - Policy tables are about 15, not 4; past bypasses came from path normalization.
  - Post-commit and event-bus semantics needed rework; only 4 of 20 setters are hooks.
  - Counts corrected; the frontend is affected.
  - The no-transaction migration mode was unsafe.
  - Bugs found: B1–B8.
- **Pass 2 (revision 2 → 3):**
  1. Auto-rollback after a migrating deploy **crash-loops** (verified). Migration discipline and boot ordering move to Phase 0a.
  2. New security bugs, both verified: the org channel list exposes webhook URLs to any member, and a chat-scoped PAT can mutate flows.
  3. B1 is latent (frames are inert), and B4 and B6 are rarer than stated. Re-ranked.
  4. Post-commit registry needs **three states**: aborted hooks must never run.
  5. Hooks need a user-scoped RLS context, because stripping it fails open.
  6. `withTx` gets a context-carrying callback.
  7. Route resolution design hardened: mount stubs, 404 vs 405, the decoded-path no-route case, a guard wrapper, and shadow-mode rollout.
  8. Phase 2b is not race-free as written; fills are now generation-checked.
  9. Narrowing the filesystem backend breaks **55 test files**; the order is now fixed.
  10. Migrations are not frozen at v31: B3 and audit `org_id` need new ones, so the loader moves to Phase 0a.
  11. Phase 0 split into 0a and 0b; RequestInfo moved to Phase 1a.
  12. Added: Postgres test harness, observability, Redis payload versioning, graceful stream drain, Redis failure modes, new-rule alert floods, rule types, AI budget and metering, generic approvals, RAG embedding identity, performance baseline, conventions docs.
  13. Corrected:
      - Flow-ID sites are 27: 21 body, 3 query, 3 path.
      - Only the scanner and retention sweep fail open on lock errors.
      - The provider `id` is already a plain string in zod; only `authType` is closed.

---

## Fix first (verified bugs, each small and independent)

| ID | Severity | Bug | Evidence | Fix |
|---|---|---|---|---|
| R1 | **High, ops** | After a deploy that ran a migration, the smoke-test auto-rollback restarts the old image. The old binary refuses a newer schema, so the app **crash-loops**. | `internal/storage/database/postgres_migrations.go:343-349`, `.github/workflows/deploy.yml:329-338` | Now: make the rollback step schema-aware (if `schema_migrations` advanced, alert instead of swapping the image). Full fix: Phase 0a migration discipline |
| S1 | Medium, security | `GET /api/orgs/{id}/channels` returns the full webhook **URL** to any org member. For Slack/Teams the URL is the credential. | `internal/api/org_channels.go:38-56` (`requireMember`) | Admin-only list, or redact the URL to host + masked path for non-admins |
| S2 | Medium, security | A PAT scoped only to `chat` can **mutate flows**: `/api/chat/fix-decision` maps to the chat scope, and `applyApprovedFix` checks only the user's flow role. | `internal/auth/apitoken.go:89`, `internal/api/handlers_chat.go:431`, `internal/service/chat_fixes.go:362,515` | Require `write` for fix-decision now; Phase 1a makes `authz.Can` check actor scopes for every mutation |
| S3 | Medium, security | Channel `url` and `secret` are plaintext `TEXT` columns. Provider keys, by contrast, use the encrypted keystore. | `postgres_migrations.go:1350-1359`, `org_channels.go:104` | Encrypt via the keystore with a resumable backfill job (after Phase 0a's loader). Local+DB mode needs R6 first |
| R2 | Medium | `releaseAdvisoryLock` ignores an unlock failure and returns the connection to the pool. The leaked lock stalls that job on every replica until the connection is recycled. | `database/postgres_lock.go:40-45` | Copy `releaseMigrationLock` (`postgres_migrations.go:406-413`): `conn.Raw(… driver.ErrBadConn)` |
| R3 | Low | `DeleteLibraryFlow`, and Library create/update, skip `notifyCloudFlowMutation`, so the dashboard is up to 30 s stale. | `service/library.go:441-513` | Call the hook now; it becomes an event in Phase 2b |
| R4 | Low | The org-invite email (up to 30 s of SMTP) runs in a post-commit hook before the buffered response is sent. | `api/org_members.go:202-206`, `middleware/compress.go:211` | Phase 0b async runner |
| R5 | Low (rare) | A failed commit after the handler wrote 2xx still sends 2xx. Triggers: the connection dies at COMMIT, or a swallowed statement error. | `api/rls_middleware.go:203-226` | Phase 0b commit-on-first-output |
| R6 | Low | In `PAD_MODE=local` + `PAD_STORAGE=database` + auth off + no `PAD_ENCRYPTION_KEY`, the DB keystore is keyed by a **per-boot random** secret, so stored keys are unreadable after a restart. | `main.go:381-389,617-624` | Resolve secrets at config load, and keep the other uses (`ProvideSecurityConfig.Token`, `writeSessionSecret` `main.go:680`) |
| R7 | Low, latent | A read-scoped PAT gets a WS ticket without scopes and can relay `block.*` frames as an editor. The frames are not persisted, and the shipped frontend ignores them. | `auth/apitoken.go:96-98`, `auth/jwt.go:188-193`, `websocket/client.go:504-519` | Carry scopes in the ticket before any live-editing feature ships (Phase 4c) |
| R8 | Medium, design | The CI webhook uses one deployment-wide HMAC secret and loads **any tenant's** flow. | `api/handlers_integrations.go:173-185` | Phase 4c per-org credentials; never hand the global secret to tenants |

---

## Findings: extensibility blockers (verified)

| # | Blocker | Evidence |
|---|---|---|
| 1 | A new handler means editing central wiring | `api.Handlers` (`internal/api/router.go:33`), `di.ProvideHandlers` with 12 params (`internal/di/api.go:90`), the `routes_chi.go:89-124` list, typed couplings (`router.go:114-119,150-155`) |
| 2 | About 15 hand-kept path tables and predicates decide policy | `publicRoutes` (`router.go:452`); `middleware_chain.go:108-236` (`rateLimitGroup`, `authRateLimitPaths`, `expensiveGetPaths`, `azureOutboundRoute`, `isOrgProviderTestPath`); `rls_middleware.go:15-149` (34 exact paths, 2 prefixes, 5 predicates); `middleware/timeout.go:23-35`; `middleware/metrics.go` `normalizeRoute`; `auth.RequiredScope` (`apitoken.go:85-105`). Group rate limits run **outside chi** (`middleware_chain.go:56-69`; the doc comment there is stale) |
| 3 | Authorization lives in handlers in about six styles | `RequireRole` ×8 inside `handlers_admin.go`; org helpers; 27 flow checks (21 read the flow ID from the body, 3 from the query, 3 from the path, `handlers_sharing.go:38,75,159`) |
| 4 | Handlers bypass services | 85 `backend.X` calls in 20 api files (plus about 23 through other store handles). Triage, comments, share tokens, policies, channels and alerts have no service |
| 5 | Storage interface | `StorageBackend` = 21 role interfaces, 115 methods; Postgres is the only live implementation; FakeBackend (1,191 lines) is not contract-tested; about 24 runtime capability assertions mix domain and Postgres-only capabilities (`database/capabilities.go:19-41`) |
| 6 | Hard-wired side effects | 4 hook setters (`di/services.go:325,360,385,427`); account-deletion fan-out in a handler (`api/auth_account.go:52-79`); cache invalidation runs **before** commit (`flow.go:534,1393`) |
| 7 | Audit is HTTP-bound and package-global | `logAudit(r, …)` (`api/audit.go:482`) at 82 sites; pool and spill are globals (`audit.go:127-180`); no `org_id`; AccessLog `user_id` is always empty |
| 8 | Implicit desktop handling | About 91 nil-backend checks and about 56–61 handler-level "cloud required" 503s |
| 9 | Data lifecycle uses central lists | Export struct (`storage.go:75-90`), retention (`postgres_retention.go:24,121,490`), migrator (`migration/migrator.go:152-380`) |
| 10 | Provider identity is spread over about 10 lists | 41 branch lines in 11 files; the frontend's closed `authType` enum (`schemas.lazy.ts:361`) and `ProviderID` union (`frontend/src/types/providers.ts:3`) |
| 11 | AI plumbing | 4 SSE parsers and 4 tool converters; hard-coded tools (`ai/tools.go:178`); a fix-only approval flow (`ai.FixProposal`, `FixProposalCard.tsx`); budget checked only at `chat.go:147` and `rag/service.go:121`; usage writes are bare goroutines, dropped when saturated and **not drained** at shutdown (`ai/audited.go:384-428`); RAG chunks lack embedding identity (`rag/service.go:286`) |
| 12 | Analyzer | Rule metadata in side tables (`engine.go:26,51,101`, `models/settings.go:250`, `incremental.go:15`, `fix_dispatch.go`). Rules run per block only (`engine.go:210-217`; flow-level rules fake it, `rule_circular_subflow.go:31-35`); all facts are built every run. Baselines store only keys (`storage.go:1124-1129`), so **a new rule raises drift/regression alerts for every baselined flow**. Custom-rule JSON is loosely decoded (`handlers_orgrules.go:86-104`) |
| 13 | Exporters | Separate switches in the CLI, API, `ExportService` and the CI webhook's own SARIF writer; the frontend renders CSV/SARIF/JUnit itself |
| 14 | Integrations | Channel kinds listed in 3 places (`notify/notify.go:53-62,106-115`); webhook signature has no timestamp or delivery ID; one method per email (`mail/service.go:93-175`) |
| 15 | Scale-out (pinned to 1 replica, `infra/main.bicep:18-26`, `deploy.yml:292`) | In-process SSE hub (`api/events.go`; `Emit` reaches **all** tenants' clients); chat begin/cancel/fix-decision; AI budget ledger; conversation read-modify-write mutex; undo ring; `uploadIDs` (`flow.go:40,477-488`); scanner alert dedup; KB lock; migration status; audit spill on `/tmp`; unversioned Redis payloads, with presence **deleted on decode failure** (`websocket/backplane.go:232-243`); readiness fails on a Redis blip (`handlers_system.go:390-394`); shutdown kills streams (`main.go:747`) |
| 16 | Deploy and migrations | Migrations run inside `storagedb.New` **before the listener binds**, so liveness can kill a long step mid-transaction; `lock_timeout=0` lets DDL queue behind the serving revision (`postgres_migrations.go:380-386`); the boot guard plus auto-rollback cause R1; checksums hash exact bytes (CRLF checkout, no `.gitattributes`) |
| 17 | Lifecycle | No job abstraction; bare goroutines; `DrainBackground` drains only `AnalysisHandler`; the scanner and retention sweep **fail open** on lock errors (`scanner.go:183-187`, `main.go:342-347`); `Bounded` logs panics at WARN, below the error sink and alert (`lifecycle/bounded.go:67`) |
| 18 | `main.go` and config | About 700 of 862 lines are providers; duration strings; stray `os.Getenv`; config mutated in DI (R6) |
| 19 | Observability | App logs lack `request_id`/`trace_id`; nothing scrapes the about 46 metrics (`main.bicep:705-707`); no instance ID; no full-chain benchmark, and nightly benchmarks are never compared |
| 20 | Tests and docs | API tests run on the **filesystem** backend through a fourth hand-wired DI copy (`api/helpers_test.go:155-240`, 211 router-helper uses); a destructive `MigrateDown` runs on the shared CI DB (`migration_v27_test.go:241`); 130 `time.Sleep` calls; e2e never reaches a Go server; no architecture docs, ADRs or CLAUDE.md |

---

## Target principles

1. **A feature is a module.** It owns its service, `api.Module`, role interface, migrations, data-lifecycle handler, event subscriptions and metrics, all contributed through fx groups.
2. **Policy is declared where the thing is defined.** Derived lists are computed.
3. **Middleware order stays the same.** The route is resolved once at the outer layer, its policy travels in `RequestInfo`, and a guard asserts that chi agrees.
4. **Layering and conventions are linted** (golangci `depguard`/`forbidigo`), and the rules are written down in `docs/architecture`.
5. **Explicit delivery semantics:**
   - post-commit hooks have three states
   - a sync invalidation tier with generation-checked fills
   - a drained async I/O tier
   - hooks run with **user-scoped** RLS; the system scope is explicit
6. **Every schema change is expand/contract.** Every Redis payload is versioned and additive.
7. **Each Redis-backed mechanism declares its failure mode** (fail closed or fail open with a TTL). Every piece of per-process state is either shared or declared replica-local.

---

## Phases

### Phase 0a: Safety nets and deploy safety (about 3–4 weeks)

**Migration discipline:**
- Each migration declares `compat: expand|breaking`. v1–v31 are classified conservatively; index-only ones such as v30 and v31 are `expand`.
- The boot guard refuses only an applied **breaking** step newer than the binary.
- Contract steps ship one release after the code stops using the old shape.
- CI job: the previous release image boots against the new schema.

**Migration loader for v32+:**
- Embedded `.sql` in one directory.
- Strip the BOM and `\r` before hashing, plus `.gitattributes` `*.sql text eol=lf`.
- Checksum golden for v1–v31.
- CI check that versions are unique.
- Each step uses `SET LOCAL lock_timeout` with retry; the advisory-lock wait stays unbounded.
- `CONCURRENTLY` indexes are idempotent ensure-steps outside the version table, checking `indisvalid` (the `postgres_vector_index.go:45-104` pattern).

**Boot ordering:**
- Bind the listener and serve `/healthz` before migrating.
- `/readyz` returns 503 until migrations finish.
- Data backfills run as resumable singleton jobs after boot.

**Postgres test harness:**
- `testutil.PG(t)`: a per-package database cloned from a migrated template, which also removes the shared-DB `MigrateDown` hazard.
- Test routers built from the **real fx modules**, with `fx.Replace` for fakes.
- `fx.ValidateApp` per mode (desktop / local+DB / cloud).

**Differential route-policy test and authz matrix** (on the Postgres harness, cloud + JWT config):
- Old classifiers run over concrete adversarial requests for every route: `/api/v1`, `/api/v1/v1/x`, `%2F`, dot segments, a trailing slash, the 405 case (e.g. `GET /api/auth/login`), HEAD/OPTIONS, `/x%2F..%2Fshared?token=`, `/assets/x`, unknown `/api/*`, `/ws`, `/swagger`, `/metrics`.
- The authz matrix covers anonymous / viewer / member / admin / scoped PAT → status.

**Contract suite on FakeBackend:** a `t.Run` skip-list wrapper plus a count golden and a strict mode. Stubs are declared unsupported.

**Lint ratchets:**
- `api` must not import `storage/database` (allowlist the 4 current files).
- Services must not import `api`.
- `BeginTx` (7 sites) and self-managed `BeginRLS(` (5 sites) are allowlisted.
- Handler `backend.X` count ratchets down from 85.
- `os.Getenv` only in config.

**Performance baseline:**
- `BenchmarkBuildHandler` through the full chain: library GET, flow save, dashboard, SSE fan-out.
- benchstat comparison in `bench-fuzz.yml`.
- Keep the `perf_statements_test.go` statement counts as the "no extra DB round trips" gate for later phases.

**Conventions:**
- `docs/architecture/` (module recipe, route policy, migrations, modes, metrics, events), `docs/adr/`, root `CLAUDE.md`.
- `capability` package plus the desktop / local+DB / cloud matrix.

**Fix-first items:** R1 (immediate rollback guard), S1, S2, R2, R3, R6.

### Phase 0b: Transaction and async foundations (about 2–3 weeks)

**`internal/platform/txhook`** owns the post-commit registry and the RLS context key:
- States are **pending → committed | aborted**.
- Committed: late registrations run inline.
- Aborted: late registrations are dropped and logged, so a blob purge never follows a rollback.
- `hasPostCommit` is false when aborted (`postgres_flows.go:163,1124` depend on it).

**`withTx(ctx, func(txCtx context.Context, tx *sql.Tx) error)`** (5 call sites):
- The fallback path puts a registry and `WithRLSTx` on `txCtx` and flushes after its own commit.
- The joined path passes `ctx` through unchanged.
- Fold in the 3 hand-rolled copies (`postgres_flows.go:87,1000`, `postgres_azure_registry.go:468`).

**Hook context:**
- `WithoutCancel` plus a **fresh lazy RLS scope for the same user**. Stripping the scope would fall back to RLS allow-all.
- The system scope is opt-in through `txhook.AsSystem(ctx, reason)`.

**Commit on first output (R5):**
- Commit at the first final `WriteHeader` (1xx ignored), the first `Write`, or `Flush`, or at handler return if nothing was written.
- On failure: restore the header snapshot taken at entry (keeps security/CORS/request-ID headers, drops handler-set cookies and ETag), write 503, fail later writes, abort the registry.
- Hooks run after the handler returns.
- Documented: a panic after the first byte leaves the write committed.

**Bounded async runner** (`lifecycle.Bounded` plus fx-stop drain):
- Panics go through `logger.RecoverPanic` at ERROR.
- Metrics: in-flight, dropped, duration.
- It replaces `runDetached` and the AI usage-metric goroutines, so they are drained at shutdown.

**Fix-first items:** R4, S3 (needs the loader).

### Phase 1a: Module seam and declarative route policy (about 4 weeks)

**`RequestInfo`:**
- Installed at the outermost layer: ID, client IP plus the raw peer IP (MetricsGuard and `RateLimitAddr` unchanged), UA, Policy.
- `Actor` is write-once (`atomic.Pointer`) and filled by `jwtAuth`, login, SSO and WS. `auth.WithClaims` is kept; the struct is never pooled.
- `logger.FromContext`; AccessLog `user_id` gets fixed.

**`api.Module`:**
- `Mount(rb)` via fx `group:"routes"`.
- The builder owns the shared prefix groups and sorts registrations.
- Delete the `Handlers` struct, `ProvideHandlers` and the `routes_chi.go` list; replace the typed couplings with small interfaces.

**Route resolution:**
- Merge `inboundRoutePath` and `dispatchPath` (`timeout.go:45-67`) into one function.
- Resolve once inside the outer Recovery, before RequestTimeout, with `mux.Find` on a **fresh** chi context that never enters the request.
- Look up policy by (method, canonical pattern):
  - A mount stub or miss means no-route.
  - A **405 policy** probes the other methods and inherits Public/RateLimitGroup.
  - The no-route policy is computed from `canonicalRequestPath(r.URL.Path)`, which is what NotFound serves.
- The outer layer never mutates `r.URL`.

**Guard:**
- Every endpoint, NotFound and MethodNotAllowed is wrapped. If chi's match disagrees with the pre-resolved policy pointer: log, metric, 500 before the handler.
- Boot self-test: each route's synthesized path resolves to itself.
- `Router.ServeHTTP` installs the policy if missing (211 test call sites).

**Rollout via `PAD_ROUTE_POLICY=legacy|shadow|declarative`:**
- Shadow mode: legacy decides, and mismatches are counted per route.
- Flip with a config-only revision.
- Delete the legacy tables one release later.
- Initial declarations are generated from the legacy classifiers.

**Metrics label = matched pattern.** Delete `staticRoutePatterns`/`normalizeRoute`; update `metrics_test.go` and dashboards (`:id` becomes `{id}`).

**Deny by default** at startup, including `/ws`, `/swagger`, `/metrics`, `/shared` and `/api/integrations/*`.

**Authorization:**
- Group middleware handles roles, org roles and the 3 path-parameter flow routes.
- The other 24 flow checks go through the typed request's `FlowRef()` or the service.
- Everything goes through `AuthzService.Can(ctx, actor, action, resource)`, which also checks **actor scopes** (closes the S2 class).

**Mode gating:** `route.Modes(...)` replaces the about 56–61 handler 503s, with the same error code.

### Phase 1b: Typed endpoints and API conventions (about 2 weeks)

- **`api.JSON[Req,Resp]`** with `Validate()` and `apperr.Error{Kind, Code, Message, Details}`, plus the `Page[T]` envelope and filter/sort conventions. Swag gets real types.
- **`Idempotency-Key`** for create endpoints (table via the 0a loader).
- **Frontend:**
  - Tolerant zod parsing for unknown enum values ships **one release before** any new value.
  - Build-version header and a reload banner for stale tabs.
- New endpoints must use the adapter; old ones migrate when touched.

### Phase 2a: Domain services and audit (about 4 weeks)

- **New services:** `TriageService`, `CommentService`, `ShareTokenService`, `PolicyService`, `ChannelService`, `GovernanceAlertService`, `AccountLifecycleService`.
  - The account service orchestrates prepare → delete → finalize, with behaviour unchanged.
  - The 85 handler calls (plus about 23 via other store handles) move into services, and the ratchet goes to 0.
  - Services take role interfaces.
- **`internal/audit`** owns the pool and spill moved out of `api`. `audit.Record(ctx, …)` reads `RequestInfo`/`Actor`.
  - Jobs record as `System(name)`.
  - `org_id` column plus a resumable backfill.
  - Login-failure audit stays a direct call.
- **Break real DI cycles** by extracting the budget checker, scope resolver and Azure model access. Document the remaining setters.

### Phase 2b: Domain events (about 2–3 weeks)

- **Bus** with no dependencies; subscriptions are made in `fx.Invoke`.
- **Typed snapshot events:** `FlowChanged{FlowID, StableID, OwnerID, OrgID, Kind}`, `CredentialChanged`, `OrgMemberChanged`, `SettingsChanged`, `KnowledgeDocChanged`, `FindingStatusChanged`.
- **Sync invalidation tier:**
  - Runs immediately, and again on commit **and** rollback.
  - Cache fills check a generation captured before the read and skip filling while a transaction is open. Without this, a stale entry survives until the TTL.
  - Redis fan-out (Phase 5b) is published only from the post-commit pass.
- **Async tier** (email, notify, SSE/WS, lent-credential disabling) runs on the 0b runner with timeout and recover per subscriber.
- **Replaces** the 4 hook setters.
- **SSE audience is mandatory** (`EmitTo`/`EmitToOrg`); broadcast needs an explicit `AudienceAll`.
- **Metrics per event:** published, failed, duration.
- Test-helper event recorder.
- Delivery is at most once; a durable outbox is deferred.

### Phase 3: Storage and data lifecycle (about 4–5 weeks)

- **Order:**
  1. Bring the fake to parity on its implemented methods (strict contract mode).
  2. Move the **55 filesystem-backed test files** onto the Postgres harness or the fake.
  3. Narrow the filesystem backend to a read-only `LegacySource`, with a read-only contract subset.
- **Capabilities:**
  - Domain capabilities (`FindingStatusGetter`, `APITokenOwnerLookup`, …) are folded into role interfaces.
  - Postgres-only capabilities (`SQLProvider`, `RLSBeginner`, `KeyStoreProvider`, `GlobalLocker`) become optional fx values. That removes `rls_middleware.go`'s import exception.
- **Per-domain files** for interfaces, Postgres and contract; `Tx(ctx, fn)`; an SQL args builder.
- **Scope the unscoped methods** (`storage.go:244,246,443`, `DeleteFindingStatus`).
- **Data-lifecycle registry** `group:"data_lifecycle"` (export, delete, purge, legacy-migrate), plus an `information_schema` coverage test. It replaces the central export struct, retention lists and migrator lists.
- **Settings** namespaced per module with a version check, plus one release of dual-read.

### Phase 4a: Analyzer and export registries (about 3 weeks)

- **`RuleMeta` on rules:** confidence, auto-fix, direct fix, defaults, related, **`IntroducedIn`**, **`Stage(preview|stable)`**. Add `RegisterFix` and a completeness test.
- **Baselines record a rule-set digest.**
  - Findings from rules newer than the baseline produce one "new rules" notice per org, instead of drift/regression alerts.
  - Preview rules never fail CI or policy gates.
- **Rule types:** `FlowRule` (whole flow) and `LibraryRule` (cross-flow), with facts computed lazily.
- **Custom-rule JSON** gets `kind` + `schemaVersion` and strict decoding.
- **Exporter registry** `{ID, MIME, Ext, Render, ServerRendered}`, used by the CLI, API, `ExportService` and the CI webhook. `bakicli` reuses `parser.ParseFolder`.
- **`cache.New(name, keyParts…)`:** tenant and rule-profile parts are required; the analyzer cache is injected.

### Phase 4b: AI providers, tools, approvals (about 4–5 weeks; start when a new provider is scheduled)

- **`ai.ProviderSpec` registry:** ID, auth type (wire enum unchanged), Caps, Modes, EgressPolicy, CredentialSchema, resolver, constructor.
  - It derives every provider list.
  - The 41 branch lines become caps or hooks.
  - Copilot and github-models special cases become spec fields.
  - Pricing comes only from the catalog.
- **Budget and metering:**
  - A pre-call **budget hook in the audited provider wrapper**, so it covers every caller.
  - Org provider allowlist enforced at factory selection.
  - Usage writes go through the drained runner.
- **Tools and approvals:**
  - Tool registry: each tool declares `RequiresApproval` and `RequiredScope`, checked against actor scopes.
  - Generic `ApprovalRequest{tool, previewKind, items, requiredScope}`, with one decision endpoint and one frontend card.
- **RAG:** chunks store the embedding identity (`ai/embedding_identity.go:26-29`) and a chunker version.
- **Frontend:** generic credential form, `ProviderID` as a string with a known set.
- **Deferred:** wire-codec rewrite, factory-scoped memo maps, generic connection registry.

### Phase 4c: Integrations and notifications (about 2 weeks)

- **Channel-kind registry** in one place, with:
  - keystore-backed secrets (S3 data already migrated)
  - egress policy per kind
  - a versioned webhook envelope with timestamp and delivery ID
- **Mail template registry.**
- **CI webhook:** per-org credentials (R8) and output through the exporter registry.
- **WS tickets carry scopes,** with a per-message check (R7).

### Phase 5a: Platform, jobs, observability (about 2 weeks)

- **Split `main.go` into fx modules.** Keep the Tauri `CONFIG:` handshake (`main.go:687-692`), the ldflags symbols (`buildstamp_test.go`) and the package-main tests.
- **`lifecycle.Singleton`:**
  - Fails **closed** on lock errors, but runs when no locker exists (desktop).
  - Metric and alert after consecutive skips (retention is a GDPR duty).
  - Heartbeat on the pinned connection; cancel on a lost lease.
  - Lock keys derived from the job name, with a uniqueness test.
  - Scanner alert dedup persisted.
- **Config:** immutable (completes R6), parsed durations.
- **Pool size per replica** = server cap / `maxReplicas`. Document that PgBouncer transaction mode breaks session locks.
- **Observability:**
  - Modules register their own metrics.
  - Prometheus scrape (managed) or KQL alerts.
  - `service.instance.id`.
  - A `secret.Value` type that redacts itself in strings, JSON and logs.
- **Graceful shutdown:** SIGTERM → `/readyz` 503, refuse new chat streams, and close open streams with `SERVER_RESTARTING` so the client resumes.

### Phase 5b: Realtime backplane and multi-replica (about 5 weeks; start only when more than one replica is needed)

- **Over Redis:**
  - SSE fan-out with event IDs for Last-Event-ID
  - subscriber presence
  - stream command channel (cancel, fix decisions)
  - budget ledger, which **fails closed** when Redis is down
- **Conversations:** an optimistic version column.
- **Other per-process state:**
  - undo ring, `uploadIDs` → shared
  - KB lock → advisory lock
  - migration status → DB
  - audit spill → per-instance file
- **Cache invalidation fan-out** with a TTL/generation safety net.
- **Redis payload envelope `{v, kind}`:** additive only; never delete on decode failure; previous-release fixture tests; frozen rate-limit group names.
- **Failure-mode table per mechanism.** `/readyz` reports Redis as *degraded*, not unready.
- **Registry test:** every service-level cache or map declares replica-local or shared.
- **Two-replica tests** in-process on miniredis and in compose. Then raise `maxReplicas`.

### Deliberately cut or deferred

- WS dispatch registry
- SSE/WS unification
- Durable outbox
- Retiring FakeBackend
- Moving v1–v31 to `.sql`
- AI wire-codec rewrite
- Typed domain events over Redis (invalidation keys only)
- Core DTO purity moves
- Expanding `/api/system/features`
- Per-org SSO
- Real API versioning (alias plus a deprecation policy is enough)

---

## Adding a feature afterwards (e.g. "flow approvals")

1. `storage/interfaces/approvals.go`, `database/postgres_approvals.go`, `contract/approvals.go`, and a fake method that passes the contract.
2. `migrations/0032_approvals.up.sql`, marked `compat: expand`, plus `.down.sql`.
3. `service/approvals.go`: `authz.Can`, `audit.Record`, `events.Publish`, metrics.
4. `api/approvals.go`: `Mount(rb)` with `route.Auth`, `Scope`, `RateLimit`, `RLS`, `Modes(Cloud)` and typed adapters.
5. A data-lifecycle handler.
6. fx registrations in the feature module.
7. Frontend types and client, with tolerant enums.

No edits to router lists, policy tables, DI param structs, cache wiring, audit plumbing, retention lists, the migrator or the metrics list.

- **A new provider:** spec file + catalog + frontend schema check.
- **A new rule:** one file with `IntroducedIn`/`Stage`.
- **A new exporter or channel kind:** one file.

## Critical files

- **HTTP:** `internal/api/{router,routes_chi,middleware_chain,rls_middleware,security,handler_helpers,org_access,org_channels,auth,auth_account,audit,audit_spill,events,handlers_chat,handlers_integrations}.go`, `internal/api/middleware/{timeout,metrics,access_log,compress,ratelimit,ip}.go`, `internal/auth/{apitoken,jwt}.go`, `internal/di/{api,services}.go`, `main.go`
- **Storage:** `internal/storage/database/{rls,postgres_lock,postgres_migrations,postgres_vector_index,postgres_flows,postgres_retention,postgres_settings,postgres_orgchannels,capabilities}.go`, `storage/interfaces/storage.go`, `filesystem/local_storage.go`, `testutil/fakebackend.go`, `storage/contract/contract.go`, `internal/migration/migrator.go`, `internal/api/helpers_test.go`
- **Domain:** `internal/service/{authz,flow,library,chat,chat_fixes,chat_resume,chat_conversation,dashboard}.go`, `internal/scanner/scanner.go`, `internal/lifecycle/*`, `internal/websocket/{client,hub,backplane}.go`
- **AI and core:** `internal/ai/{factory,registry,catalog,tools,audited,embedding_identity}.go`, `internal/rag/service.go`, `core/analyzer/{engine,registry,fix_dispatch,incremental,cache,custom_rules}.go`, `core/export/*`, `cmd/bakicli/main.go`
- **Ops:** `.github/workflows/{ci,deploy,bench-fuzz,desktop}.yml`, `infra/main.bicep`, `Dockerfile`; frontend `src/api/schemas.lazy.ts`, `src/types/providers.ts`, `FixProposalCard.tsx`

## Reuse, don't rebuild

- `TryGlobalLock` and `api.GlobalLocker`
- `releaseMigrationLock`'s bad-connection discard
- `EnsureVectorIndex`
- `WithPostCommit`/`hasPostCommit` (moved into `txhook`)
- `lifecycle.TickerLoop` and `lifecycle.Bounded`
- `core/cache.SizedLRU`
- `websocket/backplane.go` and `redisx`
- The `cached_blacklist.go` periodic reload as the lossy-pub/sub safety net
- The Redis rate limiter
- `chat_resume.go`
- `storage/contract.RunSuite`
- `route_path_test.go`
- `perf_statements_test.go`
- miniredis (already used in 4 tests)
- `ai/embedding_identity.go`
- The `registerRule` pattern
- Go 1.25 `testing/synctest` for the timing tests

## Verification

- **Every phase:**
  - `go build ./...`, `go vet`, golangci (`depguard`/`forbidigo`) and `go test ./...` in both modules on the Postgres harness. CI runs on Ubuntu with `-race`; add `timeout-minutes`.
  - gofmt on LF copies (Windows).
  - Swag diff is intentional only.
  - `buildstamp_test.go` and the package-main tests pass.
  - The **desktop boot smoke test** passes (sidecar `CONFIG:` handshake; add it to `desktop.yml`).
  - benchstat shows no regression over 5% on the `BuildHandler` benchmarks, and the statement-count tests are unchanged.
- **Phase 0a:**
  - The previous-release-boots-on-new-schema job passes.
  - The checksum golden passes; CRLF/BOM `.sql` files hash like LF.
  - A long migration is not killed by liveness.
  - The R1 rollback guard is tested in a dry-run workflow.
  - Tests for S1 (member gets a redacted list), S2 (chat-only PAT → 403 on fix-decision) and R2 (unlock failure discards the connection).
- **Phase 0b:**
  - An aborted registry drops late hooks.
  - A hook DB access runs under user RLS.
  - A commit failure produces 503 with restored headers.
  - The runner drains at shutdown.
  - The usage metric survives SIGTERM.
- **Phase 1a:**
  - The differential golden is unchanged.
  - The authz matrix is unchanged.
  - Shadow mode reports zero mismatches over a soak period before the flip.
  - The guard self-test and deny-by-default check pass.
- **Phase 2:**
  - Library and Flow mutations invalidate the same caches.
  - The generation check blocks stale fills.
  - Audit rows keep IP/UA and add actor and org.
- **Phase 3:** contract strict mode is green; the data-lifecycle coverage test passes; the existing DB (live v29 per notes, with v30–v31 pending) boots cleanly.
- **Phase 4:**
  - Registry completeness (providers, rules, exporters, tools).
  - A new preview rule raises no drift alerts and doesn't gate.
  - The frontend handles an unknown provider and auth type.
- **Phase 5:** two replicas on miniredis and in compose:
  - an SSE stream crosses replicas, and cancel works from the other replica
  - the budget cap holds
  - jobs run once with no duplicate alerts after failover
  - mixed-version Redis payloads decode
  - Redis down → budget fails closed and the rest degrades
- **Docker smoke test:** login, import and analyze, chat with a tool call, Azure provider list, admin users page, CI webhook.
- Leave every change uncommitted for the user.

## Order

1. **R1, S1, S2, R2** (days)
2. **0a → 0b → 1a → 1b → 2a → 2b** (Tier 1, about 5 months)
3. **3 → 4a → 4c → 5a** (Tier 2)
4. **4b and 5b** when their trigger happens. Phase 5b is the only gate for raising `maxReplicas`.
