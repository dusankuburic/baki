# Deployment profiles

The backend runs in three profiles. `internal/capability` derives the profile and its capabilities from the
validated config, and `capability_test.go` pins this matrix.

| Profile | How it is configured | Server store | Accounts | Row-level security | Shared state (Redis) |
|---|---|---|---|---|---|
| `desktop` | Tauri sidecar: `PAD_MODE=local`, no `PAD_STORAGE` (nil storage backend) | – | – | – | – |
| `local+db` | `PAD_MODE=local`, `PAD_STORAGE=database` (development) | ✓ | when `PAD_AUTH_ENABLED=true` | when auth is enabled | when `PAD_REDIS_URL` is set |
| `cloud` | `PAD_MODE=cloud` (auth required by config validation) | ✓ | ✓ | ✓ | when `PAD_REDIS_URL` is set |

Capabilities (`capability.Capability`):

- `server-store`: a persistent server-side store (Postgres) for library, orgs, sharing, analytics and audit.
- `accounts`: multiple users authenticate (JWT sessions, API tokens, SSO). Without it there is one local user, with
  no `users` row even on Postgres: their settings are the deployment's (`GET/POST /api/system/settings` act on the
  empty key scope), as on the desktop.
- `row-level-security`: requests run in a per-user RLS transaction. This needs both a store and accounts.
- `shared-state`: a Redis backplane shares rate limits, presence, chat-stream resume and the commands that act on a running stream (stop, approval decision) across replicas. An unreachable Redis degrades it (the app runs on, `/readyz` lists `redis` under `degraded`) instead of refusing the boot; `PAD_MAX_REPLICAS` above 1 requires it ([ADR 0029](../adr/0029-redis-envelope-key-registry-and-health.md)).

The desktop profile keeps cloud features off by design. Features that need the server store say so through
the capability, instead of checking for a nil backend inline.

The daily AI budget is one of them in effect, not by a check: without a usage store there is no usage recorder
(`ProvideAI` leaves it nil) and today's spend reads $0 (`AIBudget` with a nil `DailyUsageReader`), so the cap
Settings shows is enforced only against the in-flight ledger of running chat streams (ADR 0028). The provider
policy applies its deployment rule there too; the organisation allowlist needs org settings, which need the
store.

Two defaults differ by profile. In `cloud`, Postgres requires TLS unless `PAD_DB_REQUIRE_SSL` says otherwise
(`internal/app`'s `storageConfig` asks the profile), and a stop pauses `PAD_SHUTDOWN_DRAIN_DELAY`, 3 s, before it
ends the open streams (0 in the others). `config.Load` sets that one from the mode, the fact the `cloud` profile is
derived from, because `internal/config` cannot import `capability`; `TestDrainDelayDefault_FollowsTheCloudProfile`
keeps the two in step.

Routes do the same with `route.Needs(capability.ServerStore, ...)`. When the process lacks a capability the
route needs, the route answers 503 `SERVICE_UNAVAILABLE` before its handler runs, in every `PAD_ROUTE_POLICY`
mode. That is the same answer as the handlers' own "cloud mode required" checks, which move onto their routes
route by route. The router takes its capabilities from `capability.Of(cfg)`. Tests give a router other ones with
`capability.NewSet`, since test routers run on the test fake backend with the default config.

The CI webhook (`POST /api/integrations/ci`, [ADR 0019](../adr/0019-org-ci-credentials.md)) shows the pattern:

| Profile | CI webhook |
|---|---|
| `desktop` | 503: the route needs the server store. |
| `local+db` without accounts | Authenticated by the server's `PAD_CI_WEBHOOK_SECRET` alone; without it, 503. The flow is resolved by id from the store. |
| With accounts (`cloud`, or `local+db` with auth) | Organisation CI credentials, minted by org admins (`/api/orgs/{id}/ci-credentials`, which need `server-store` and `accounts`). A credential reaches its organisation's flows only. A personal API token with the `ci` scope (`Authorization: Bearer`) reaches the flows its user can read, personal ones included (an organisation's flow only for its members). `PAD_CI_WEBHOOK_SECRET` still works but is deprecated: a boot warning, and every answer to it carries `Deprecation: true` and a `Link` header. |

Running more than one replica is a separate question from the profile: it needs `shared-state` (the app refuses
`PAD_MAX_REPLICAS` above 1 without `PAD_REDIS_URL`), and the repository still ships one replica without Redis
(`infra/main.bicep`) until the two-replica CI job has been green; the capability is a repository variable away. The
checklist, what is shared and what each mechanism does when Redis is down or a replica dies are in
[operations.md](operations.md#running-more-than-one-replica).

Some per-process state already follows the profile. The undo ring of a stored flow (the pre-mutation source that "undo
fix" restores) lives in the `flow_snapshots` table wherever the profile has `server-store`, so any replica can undo
what another applied; the desktop keeps it in memory, as it also does for the files and folders it opens in every
profile ([data-lifecycle.md](data-lifecycle.md#undo-ring-flow_snapshots-migration-0045)). `di.snapshotStoreOptions`
asks the capability, not the mode.
