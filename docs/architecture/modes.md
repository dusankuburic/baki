# Deployment profiles

The backend runs in three profiles; `internal/capability` derives the profile and its capabilities from the validated config (`capability_test.go` pins the matrix).

| Profile | Configured | Server store | Accounts | Row-level security | Shared state (Redis) |
|---|---|---|---|---|---|
| `desktop` | Tauri sidecar: `PAD_MODE=local`, no `PAD_STORAGE` (nil storage backend) | – | – | – | – |
| `local+db` | `PAD_MODE=local`, `PAD_STORAGE=database` (development) | ✓ | when `PAD_AUTH_ENABLED=true` | when auth is enabled | when `PAD_REDIS_URL` is set |
| `cloud` | `PAD_MODE=cloud` (auth required by config validation) | ✓ | ✓ | ✓ | when `PAD_REDIS_URL` is set |

Capabilities (`capability.Capability`): `server-store` (a persistent Postgres for library, orgs, sharing, analytics and audit), `accounts` (multiple users authenticate — JWT sessions, API tokens, SSO; without it there is one local user with no `users` row even on Postgres, whose settings are the deployment's), `row-level-security` (a per-user RLS transaction; needs both a store and accounts), `shared-state` (a Redis backplane for rate limits, presence, chat-stream resume and the commands that act on a running stream; unreachable degrades — the app runs on, `/readyz` lists `redis` under `degraded` — instead of refusing; `PAD_MAX_REPLICAS` above 1 requires it, [ADR 0029](../adr/0029-redis-envelope-key-registry-and-health.md)). Features that need the server store say so through the capability, never a nil-backend check. The org allowlist is one in effect: it needs org settings, which need the store (ADR 0028).

Four defaults differ in `cloud`: Postgres requires TLS unless `PAD_DB_REQUIRE_SSL` says otherwise; a database role that bypasses row-level security refuses to boot unless `PAD_DB_ALLOW_BYPASSRLS`; the stop pauses `PAD_SHUTDOWN_DRAIN_DELAY`, 3 s, before ending the open streams; and the retention sweep keeps the audit trail and the other windowed tables `PAD_AUDIT_RETENTION_DAYS`, 400 days (0, the other profiles' default, keeps them forever). The last two are set by `config.Load` from the mode — the fact the `cloud` profile is derived from — because `internal/config` cannot import `capability` (`TestDrainDelayDefault_FollowsTheCloudProfile`, `TestAuditRetentionDefault_FollowsTheCloudProfile` keep them in step).

A route needing a capability the process lacks (`route.Needs(capability.ServerStore, ...)`) answers 503 `SERVICE_UNAVAILABLE` before its handler runs, in every `PAD_ROUTE_POLICY` mode — the same answer as the handlers' own "cloud mode required" checks, which move onto their routes. The router takes its capabilities from `capability.Of(cfg)`; tests give a router others with `capability.NewSet`. The pattern, with the CI webhook (`POST /api/integrations/ci`, [ADR 0019](../adr/0019-org-ci-credentials.md)):

| Profile | CI webhook |
|---|---|
| `desktop` | 503: the route needs the server store. |
| `local+db` without accounts | Authenticated by the server's `PAD_CI_WEBHOOK_SECRET` alone; without it, 503. |
| With accounts (`cloud`, or `local+db` with auth) | Organisation CI credentials, minted by org admins (`/api/orgs/{id}/ci-credentials`), reaching their organisation's flows only; or a personal API token with the `ci` scope (`Authorization: Bearer`), reaching the flows its user can read. `PAD_CI_WEBHOOK_SECRET` still works but is deprecated (a boot warning; `Deprecation: true` and a `Link` header on every answer). |

Running more than one replica is a separate question from the profile: it needs `shared-state` (the app refuses `PAD_MAX_REPLICAS` above 1 without `PAD_REDIS_URL`), and the repository ships one replica without Redis until the `two-replica` CI job has been green — the checklist and failure modes are in [operations.md](operations.md#running-more-than-one-replica). The undo ring of a stored flow follows the profile too: the `flow_snapshots` table wherever there is `server-store`, the in-memory ring otherwise ([data-lifecycle.md](data-lifecycle.md#undo-ring-flow_snapshots-migration-0045)); `di.snapshotStoreOptions` asks the capability, not the mode.
