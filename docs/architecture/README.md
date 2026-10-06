# Backend architecture notes

How the backend is put together and the rules new code follows. The refactoring that moves the code toward
these rules is planned in [backend-refactoring-proposal.md](backend-refactoring-proposal.md). Decisions with
lasting consequences are recorded as ADRs in [`../adr/`](../adr/).

| Topic | Where |
|---|---|
| Deployment profiles and what each can do | [modes.md](modes.md) |
| Schema migrations (format, compat, rollback) | [`internal/storage/database/migrations/README.md`](../../internal/storage/database/migrations/README.md) |
| Test safety nets (goldens, ratchets, isolated databases, benchmark) | [safety-nets.md](safety-nets.md) |
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
- **Post-commit work.** Anything that must happen only once a write is durable (blob purges, emails, cache
  invalidation) goes through `txhook.Register`. It runs after the commit, is dropped on a rollback, and gets a
  context with its own RLS scope for the same user (ADR 0004).
- **Responses.** The request's transaction commits before the first byte of the response. Once a handler has
  started writing, it must not touch the database again (ADR 0004).
- **Background work.** Anything that must not hold up the request runs on a `lifecycle.NewBounded` runner,
  which is capped, reports panics and is drained at shutdown, never a bare `go func`.
- **Secret columns.** Credentials stored outside the keystore are sealed with `sealField`/`openField` and
  included in the at-rest backfill (ADR 0005).
- **Configuration.** Read environment variables only in `internal/config`. Resolve secrets once at load
  (`config.ResolveRuntimeSecrets`); providers never fill in config later (`internal/archtest`).
- **Deployment modes.** Ask `capability.Of(cfg)` what the process can do; don't compare `cfg.Mode` or test for a
  nil backend.
- **Migrations.** New steps are `.sql` files with a `-- baki:compat expand|breaking` line. Prefer expand/contract.
  Never edit a shipped step (its checksum is pinned).
- **Route policy.** Public routes, scopes, rate-limit buckets, RLS exemptions, deadlines and metric labels are
  still path tables (Phase 1a moves them onto the routes). Any change to them shows up in
  `internal/api/testdata/route_policy.golden`, so review that diff.
