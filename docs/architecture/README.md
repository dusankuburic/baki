# Backend architecture notes

How the backend is put together and the rules new code follows. The refactoring that moves the code toward
these rules is planned in [backend-refactoring-proposal.md](backend-refactoring-proposal.md). Decisions with
lasting consequences are recorded as ADRs in [`../adr/`](../adr/).

| Topic | Where |
|---|---|
| Deployment profiles and what each can do | [modes.md](modes.md) |
| Operating a deployment: database connections, pooling, the stop budget, running more than one replica, alerts | [operations.md](operations.md) |
| Schema migrations (format, compat, rollback) | [`internal/storage/database/migrations/README.md`](../../internal/storage/database/migrations/README.md) |
| Test safety nets (goldens, ratchets, isolated databases, benchmark) | [safety-nets.md](safety-nets.md) |
| DI wiring and the post-construction setters that remain | [di-wiring.md](di-wiring.md) |
| What the export, erasure, organisation deletion and retention do with each table | [data-lifecycle.md](data-lifecycle.md) |
| How a storage write gets its transaction (`withTx` and its variants) | [transactions.md](transactions.md) |
| The analyzer: rule kinds, the walk, lazy facts, adding a rule | [analyzer.md](analyzer.md) |
| Per-package test map | [`../testing.md`](../testing.md) |

## Rules for new code

These rules are enforced by tests. Today's exceptions are pinned, and the tests fail when an exception grows
and when it shrinks, so the pinned numbers keep going down.

- **Layers.**
  - `internal/api` reaches storage through services and the `storage/interfaces` role interfaces, never the
    Postgres package (`internal/archtest`).
  - `internal/service` never imports `internal/api`.
  - `core` is a separate module, and `internal` is out of its reach by construction.
- **Transactions.** Writes go through `withTx` (`internal/storage/database/rls.go`), which joins the request's
  RLS transaction, or a variant of it: `withTxAs` opens its own transaction as the user when there is no request
  transaction, `withOwnTx` never joins (writes that must outlive the request's rollback). A raw `BeginTx` beside
  them takes a second connection and runs without RLS (`internal/archtest`, [transactions.md](transactions.md)).
- **Tenant scoping.** Row-level security limits a table to the rows the caller can see; roles are the service's
  (`AuthzService.Can`). A new table gets a policy or a reason in `TestRLS_TablesWithoutRowSecurityAreListed`, and a
  read across tenants (`FlowFilter.AllFlows`, the scanner's batch reads) stays in the jobs
  `TestArch_SystemReadsPinned` lists (ADR 0013).
- **Handlers.** New endpoints do not call the storage backend directly. Put the logic in a service: that is
  where authorization, audit and cache invalidation belong. The count of existing direct calls only goes down
  (`internal/archtest`).
- **Typed endpoints.** New endpoints are built with `api.JSON` (a JSON body) or `api.Query` (the URL only) from a
  function of a typed request to a typed response. The request validates itself (`Validate`), names the flow it
  acts on (`FlowRef`, authorized with `AuthorizeFlow`), and pages through an embedded `PageRequest` into a
  `Page[T]`. Errors are `apperr.Error` values or the service sentinels, written by `render.Error`. Existing
  handlers move over when they are touched, and the count of hand-written routes per file only goes down
  (`internal/archtest`). The conventions are in [api-conventions.md](api-conventions.md).
- **Post-commit work.** Anything that must happen only once a write is durable (blob purges, emails, cache
  invalidation) goes through `txhook.Register`. It runs after the commit, is dropped on a rollback, and gets a
  context with its own RLS scope for the same user (ADR 0004).
- **Responses.** The request's transaction commits before the first byte of the response. Once a handler has
  started writing, it must not touch the database again (ADR 0004).
- **Wiring.** A service takes its dependencies in its constructor. A post-construction `Set*`/`On*` call in
  `internal/di` is allowed only for a cycle or a listed deferral, and every one is pinned with its reason
  (`TestDISetters_Listed`, [di-wiring.md](di-wiring.md)). When two services need each other, extract the part both
  need, as `AIBudget` and `FlowScopeResolver` were.
- **Events.** A change another module derives something from is published as a domain event
  (`internal/events`, ADR 0009) by the service that made it, inside its transaction; the deriving module subscribes
  from an `fx.Invoke`. Cache invalidation is a sync subscriber, and the cache's fills are generation-checked
  (`eventbus.Gen`); side effects (email, realtime, notifications) are async subscribers, which run only after the
  commit. Never call into another service to tell it something changed, and never add a hook setter for it.
  Every subscriber states whether another replica must reproduce its effect: `eventbus.Local(reason)` (a side
  effect that must happen once, or nothing that can go stale across replicas) or `eventbus.Replicated(...)` (it
  publishes invalidation keys once the write is durable). There is no default; every subscriber is listed in
  `internal/di/testdata/subscribers.golden` ([safety-nets.md](safety-nets.md#event-subscribers-internalditestdatasubscribersgolden)).
  With Redis configured, `internal/invalidation` carries a replicated subscriber's keys (never typed events) to the other
  replicas on `baki:inval`, where each key's applier (`Bus.OnRemote`) runs the cache owner's own drop; a change that is
  not a domain event (an analysis recorded) announces itself with `Bus.Invalidate`. A missed message is healed by an
  epoch gap check, a resync after every resubscription and the cache's TTL (ADR 0032).
- **Server-sent events.** `api.EventManager` delivers to this replica's clients and, when the deployment has the
  shared Redis, to the other replicas' through `api.EventFanout` (`redisx.SSEBackplane`; ADR 0030): an event is
  delivered locally, then published once, and a receiver delivers locally without publishing. Durable events get an SSE
  `id:` from a short per-user Redis Stream and a reconnect with `Last-Event-ID` is replayed from it; chat chunks and
  progress ticks are published only for users another replica holds. `HasSubscriber` consults the per-replica presence
  records and answers true when it cannot read them. Without Redis none of this exists and nothing changes.
- **Caches.** A cache is declared once, as a package-level `cache.Declare(name, cache.Keys(...), ...)`: its key
  parts include a tenant unless `cache.Global(reason)` says why not, and it states where its entries live
  (`cache.ReplicaLocal(reason)`, or `cache.Shared(cache.MechanismInvalidation, reason)` when the invalidation fan-out
  drops it on every replica). Build its storage with `cache.New` and fill it through `GetOrFill`, which flies
  on the full key. Any other per-process map, list or cache beside a lock is declared in `stateRegistry`
  (`TestArch_PerProcessStateDeclared`, ADR 0032). Raw LRU constructions and `singleflight.Group`s are pinned per file (`internal/archtest`), and
  every declaration is in `internal/di/testdata/caches.golden` (ADR 0017).
- **Background work.** Anything that must not hold up the request runs on a `lifecycle.NewBounded` runner,
  which is capped, reports panics and is drained at shutdown, never a bare `go func`.
- **Metrics.** A package defines its own metrics beside the code that records them and registers them with
  `metrics.MustRegister` from a package-level var or `init`, never at construction and never on the client
  library's global registry (`TestRegistration_OnlyThroughThePackageRegistry`). A package that starts registering
  is blank-imported in `internal/metrics/registry_external_test.go`, and every family is listed with its owner in
  `internal/metrics/testdata/metrics.golden` (refresh: `go test ./internal/metrics/ -run TestMetricsGolden
  -update-metrics`, and review the diff: dashboards and alerts name these). The central list in `internal/metrics`
  only shrinks (`centralListPin`).
- **Request logs.** Log on the request path through `logger.FromContext(ctx)`, so the line carries the
  `request_id`, `user_id` and `trace_id` of the request's access-log line. They come from the request's
  `reqinfo.Info`, which `middleware.RequestInfo` installs at the outermost layer. Whoever authenticates the
  request records its actor there, once: `jwtAuth` (sessions, API tokens, local mode's user), login,
  registration that signs in, refresh, the SSO callback and exchange, and the WebSocket upgrade.
- **Audit.** Record through an injected `*audit.Recorder`: `Record(ctx, audit.Entry{…, OrgID})`. The actor comes from
  the context (a background job sets `auth.SystemActor(name)`), the address and user agent from `reqinfo`.
  `internal/audit` never imports `internal/api` (`internal/archtest`, ADR 0008).
- **Data lifecycle.** Every table is claimed in a `lifecycle_<domain>.go` file of `internal/storage/database`:
  what the data-subject export, account erasure, organisation deletion and the retention sweep do with it
  (`TestPostgres_Lifecycle_EveryTableClaimed`, ADR 0011, [data-lifecycle.md](data-lifecycle.md)).
- **Secret columns.** Credentials stored outside the keystore are sealed with `sealField`/`openField` and
  included in the at-rest backfill and in `fieldSealedColumns`, which the key-rotation re-seal pass walks (ADR 0005,
  `TestReseal_CoversEverySealedColumn`).
- **Configuration.** Read environment variables only in `internal/config`. `config.Load` builds the
  configuration once: durations parsed (`config.Duration`), ranges validated on every source, secrets, aliases and
  the instance id resolved. Providers share the one `*config.Config` and never write to it
  (`TestArch_ConfigNotMutatedOutsideConfig` in `internal/archtest`).
- **Secrets in configuration.** Credentials, and the values that carry one (a DSN, a webhook URL), travel as
  `secret.Value` (`internal/platform/secret`), which prints, marshals and logs as `[redacted]`, and leave only
  through `Reveal()` at the boundary that needs the plain text (a driver, a signer, the keystore), never into a log
  or an error. A struct that keeps one for the life of the process keeps the `secret.Value` and compares with
  `Equal` (ADR 0005, `TestConfig_FormatAndJSONNeverShowSecrets`).
- **Deployment modes.** Ask `capability.Of(cfg)` what the process can do; don't compare `cfg.Mode` or test for a
  nil backend.
- **AI providers.** A provider is described once, as an `ai.ProviderSpec` in `internal/ai/spec_<id>.go`; every
  provider list is derived from `ai.Specs()` (the factory, the keystore keys, the embedding order, the listing, the
  catalogue route, the pricing fallback), and `internal/ai/testdata/providers.golden` pins the table (refresh with
  `-update-provider-specs`). Never branch on a provider id by name: ask the spec (`Caps`, `Keys`, `Modes`,
  `Pricing`, `Listing`); the literal count per file only goes down (`internal/archtest`). A family with its own
  registry installs a `ListingHook` from DI (ADR 0025, [ai-providers.md](ai-providers.md)).
- **Chat tools.** A tool the model can call is an `ai.ToolSpec` in `internal/ai/tools.go`, which says whether it
  needs an approval, which token scope it needs and how the card previews it (`internal/ai/testdata/tools.golden`,
  refresh with `-update-tools`). The stream's `ai.ToolGate` decides what is offered AND is asked again in
  `ExecuteTool`; never test a tool name in the service, ask `ai.ToolRequiresApproval`. A tool that must ask the user
  goes through `ChatService.requestApproval` (`chat_approvals.go`) and the one decision endpoint (ADR 0026).
- **Chat conversations.** In cloud mode a stored conversation has a `version` the database moves on every change of its
  messages (migration 0044, a trigger and one sequence, never reused). The server's own writers (the user turn at
  stream start, the assistant answer at stream end) go through `ChatService.mutateConversation`: read with the
  version, change, write with `SaveConversationIf`, and read again on a conflict, so a second replica's turn lands
  next to the first instead of replacing it. A client save may carry `baseVersion` for the same check (409
  `CONVERSATION_CONFLICT`); one that sends none stays a blind replace, counted in `pad_chat_blind_saves_total`. The
  per-conversation mutex stays as a queue for one replica's writers, not as the correctness mechanism; local modes
  keep files and have no versions (ADR 0031).
- **Chat stream commands.** A chat stream lives on the replica that started it. A stop or an approval decision that lands on
  another replica goes through `ChatService.CancelStreamAs` / `DecideApproval` (`chat_commands.go`): the resume snapshot
  names the owner (`replica`), the request is published on the owner's `baki:chat:cmd:<instance>` channel through
  `redisx.CommandBus` and answered on the asker's reply channel, and the owner repeats the ownership and scope checks
  before it acts. With Redis unreachable the answer is 503 `REPLICA_UNREACHABLE`, never an ok; a stream on the replica
  that got the request uses no Redis (ADR 0033). New stream-scoped endpoints that mutate a running stream take the same
  path: never read `activeStreams` for a stream that may belong to another replica.
- **Storage capabilities.** Never type-assert a store to learn what it can do (`internal/archtest`, ADR 0010). A
  method any store could offer joins its role interface, and every backend implements it under the storage
  contract. What only the database offers (the cross-replica lock, the request transaction, the keystore, …) is an
  fx value `internal/app` provides beside the backend, nil without a database: take it as a parameter.
- **Migrations.** New steps are `.sql` files with a `-- baki:compat expand|breaking` line. Prefer expand/contract.
  Never edit a shipped step (its checksum is pinned).
- **Settings.** Each top-level key of `AppSettings` is a section with its own version, declared in
  `internal/storage/settingsdoc/sections.go` (`TestSections_CoverTheDocument`). A change of a section's stored
  shape that decoding cannot absorb raises its version and adds an upgrade; a field never changes its JSON type.
  The stores keep what a newer binary wrote (ADR 0012).
- **Routes.** A feature mounts its own routes: its handler implements `api.Module` (`Mount(rb *route.Builder)`),
  joins fx's `routes` group in `internal/di`, and declares each route's policy where it registers the route.
  There is no central route list, and the router refuses to build when a route was registered around the
  Builder. While the legacy path tables remain, `TestDeclaredPolicies_MatchLegacy` keeps the declarations equal
  to them.
- **Route policy.** A route declares its policy where it is registered (`route.Builder`): who may call it, the
  token scopes, the rate-limit bucket, the RLS transaction, the deadline class and the capabilities it needs. The
  router resolves each request to that policy once, at the outer layer, and every middleware reads it. A route
  without a policy does not boot, and a request chi dispatches elsewhere than resolved never reaches a handler in
  declarative mode (ADR 0006). The legacy path tables decide until `PAD_ROUTE_POLICY=declarative`.
  - Never add a path to a middleware table: declare the option on the route.
  - A route that creates a record declares `route.Idempotent()`, so a retry with an `Idempotency-Key` gets the
    first response back instead of creating a second record (ADR 0007).
  - Review any change in `internal/api/testdata/route_policy_declared.golden`, and in `route_policy.golden`
    while the tables remain.

## Old clients against a new server

A browser tab outlives a deploy, so the frontend that reads a response may be one release older than the server.

- **New enum values.** A new value of an enum the backend sends ships one release after the frontend's tolerant
  parsing that accepts it (`tolerantEnum` in `frontend/src/api/schemas.lazy.ts`, `Tolerant<T>` in
  `frontend/src/types/tolerant.ts`). A closed `z.enum` fails the whole response on one unknown value; only the
  roles and `Finding.severity` stay closed on purpose. A new enum field is tolerant from its first release.
- **Provider catalogue.** AI Accounts renders its sections from `GET /api/providers/catalog`; a server without the
  route (404) gets the frontend's built-in sections (`LegacyProvidersPanel`), and a provider, credential kind or
  field type the frontend does not know renders a generic card (configured state, help link, Test/Remove), never a
  guessed input.
- **Stale tabs.** Every response carries the running build in `X-Baki-Version` (`securityHeaders`); the frontend
  compares it with the build in `index.html`'s `baki-version` meta, which the image build stamps after bundling so
  no chunk is renamed by a new version, and offers a reload once per server build when they differ. The version is
  public on purpose: the page names it anyway, and anonymous responses carry it so a tab compares on its first call.
