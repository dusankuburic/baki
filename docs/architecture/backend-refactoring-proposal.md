# Backend refactoring proposal: making Baki extensible (revision 3)

The refactor (revision 3, two review passes) completed on 2026-10-16: phases 0a–5b are done, and the rules they
established live in [README.md](README.md) and the pages it links ([safety-nets.md](safety-nets.md),
[transactions.md](transactions.md), [data-lifecycle.md](data-lifecycle.md), [di-wiring.md](di-wiring.md),
[api-conventions.md](api-conventions.md), [operations.md](operations.md)). Phase 5b's code (`RedisBackplane`,
rolling-deploy overlap, and `bakicli migrate` maintenance commands) is implemented; flipping the deployment switch to
more than one replica remains gated on needing it (the repository ships `maxReplicas = 1` without Redis, and raising
the count follows [operations.md](operations.md#running-more-than-one-replica)). This page stays for the recipe and the
file map below.

## Adding a feature afterwards (e.g. "flow approvals")

1. `storage/interfaces/approvals.go`, `database/postgres_approvals.go`, `contract/approvals.go`, and a fake method
   that passes the contract.
2. `migrations/0032_approvals.up.sql`, marked `-- baki:compat expand`, plus `.down.sql`.
3. `service/approvals.go`: `authz.Can`, `audit.Record`, `events.Publish`, metrics.
4. `api/approvals.go`: `Mount(rb)` with `route.Auth`, `Scope`, `RateLimit`, `RLS`, `Modes(Cloud)` and typed adapters.
5. A data-lifecycle handler.
6. fx registrations in the feature module.
7. Frontend types and client, with tolerant enums.

No edits to router lists, policy tables, DI param structs, cache wiring, audit plumbing, retention lists, the
migrator or the metrics list.

- **A new provider:** spec file + catalog + frontend schema check.
- **A new rule:** one file with `IntroducedIn`/`Stage`.
- **A new exporter or channel kind:** one file.

## Critical files

- **HTTP:** `internal/api/{router,routes_chi,middleware_chain,rls_middleware,security,handler_helpers,org_access,org_channels,auth,auth_account,audit,audit_spill,events,handlers_chat,handlers_integrations}.go`,
  `internal/api/middleware/{timeout,metrics,access_log,compress,ratelimit,ip}.go`, `internal/auth/{apitoken,jwt}.go`,
  `internal/di/{api,services}.go`, `main.go`
- **Storage:** `internal/storage/database/{rls,postgres_lock,postgres_migrations,postgres_vector_index,postgres_flows,postgres_retention,postgres_settings,postgres_orgchannels,capabilities}.go`,
  `storage/interfaces/storage.go`, `filesystem/local_storage.go`, `testutil/fakebackend.go`,
  `storage/contract/contract.go`, `internal/migration/migrator.go`
- **Domain:** `internal/service/{authz,flow,library,chat,chat_fixes,chat_resume,chat_conversation,dashboard}.go`,
  `internal/scanner/scanner.go`, `internal/lifecycle/*`, `internal/websocket/{client,hub,backplane}.go`
- **AI and core:** `internal/ai/{factory,registry,catalog,tools,audited,embedding_identity}.go`, `internal/rag/service.go`,
  `core/analyzer/{engine,registry,fix_dispatch,incremental,cache,custom_rules}.go`, `core/export/*`, `cmd/bakicli/main.go`
- **Ops:** `.github/workflows/{ci,deploy,bench-fuzz,desktop}.yml`, `infra/main.bicep`, `Dockerfile`; frontend
  `src/api/schemas.lazy.ts`, `src/types/providers.ts`, `FixProposalCard.tsx`
