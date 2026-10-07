# Backend architecture notes

How the backend is put together and the rules new code follows. The refactoring that moves the code toward
these rules is planned in [backend-refactoring-proposal.md](backend-refactoring-proposal.md). Decisions with
lasting consequences are recorded as ADRs in [`../adr/`](../adr/).

| Topic | Where |
|---|---|
| Deployment profiles and what each can do | [modes.md](modes.md) |
| Schema migrations (format, compat, rollback) | [`internal/storage/database/migrations/README.md`](../../internal/storage/database/migrations/README.md) |
| Test safety nets (goldens, ratchets, isolated databases, benchmark) | [safety-nets.md](safety-nets.md) |
| DI wiring and the post-construction setters that remain | [di-wiring.md](di-wiring.md) |
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
  RLS transaction. A raw `BeginTx` beside it takes a second connection and runs without RLS (`internal/archtest`).
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
  `internal/di` is allowed only for a cycle, an event hook (until Phase 2b's bus), or a listed deferral, and
  every one is pinned with its reason (`TestDISetters_Listed`, [di-wiring.md](di-wiring.md)). When two services
  need each other, extract the part both need, as `AIBudget` and `FlowScopeResolver` were.
- **Background work.** Anything that must not hold up the request runs on a `lifecycle.NewBounded` runner,
  which is capped, reports panics and is drained at shutdown, never a bare `go func`.
- **Request logs.** Log on the request path through `logger.FromContext(ctx)`, so the line carries the
  `request_id`, `user_id` and `trace_id` of the request's access-log line. They come from the request's
  `reqinfo.Info`, which `middleware.RequestInfo` installs at the outermost layer. Whoever authenticates the
  request records its actor there, once: `jwtAuth` (sessions, API tokens, local mode's user), login,
  registration that signs in, refresh, the SSO callback and exchange, and the WebSocket upgrade.
- **Audit.** Record through an injected `*audit.Recorder`: `Record(ctx, audit.Entry{…, OrgID})`. The actor comes from
  the context (a background job sets `auth.SystemActor(name)`), the address and user agent from `reqinfo`.
  `internal/audit` never imports `internal/api` (`internal/archtest`, ADR 0008).
- **Secret columns.** Credentials stored outside the keystore are sealed with `sealField`/`openField` and
  included in the at-rest backfill (ADR 0005).
- **Configuration.** Read environment variables only in `internal/config`. Resolve secrets once at load
  (`config.ResolveRuntimeSecrets`); providers never fill in config later (`internal/archtest`).
- **Deployment modes.** Ask `capability.Of(cfg)` what the process can do; don't compare `cfg.Mode` or test for a
  nil backend.
- **Migrations.** New steps are `.sql` files with a `-- baki:compat expand|breaking` line. Prefer expand/contract.
  Never edit a shipped step (its checksum is pinned).
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
- **Stale tabs.** Every response carries the running build in `X-Baki-Version` (`securityHeaders`); the frontend
  compares it with the build in `index.html`'s `baki-version` meta, which the image build stamps after bundling so
  no chunk is renamed by a new version, and offers a reload once per server build when they differ. The version is
  public on purpose: the page names it anyway, and anonymous responses carry it so a tab compares on its first call.
