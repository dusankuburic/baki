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
- `accounts`: multiple users authenticate (JWT sessions, API tokens, SSO). Without it there is one local user.
- `row-level-security`: requests run in a per-user RLS transaction. This needs both a store and accounts.
- `shared-state`: a Redis backplane shares rate limits, presence and chat-stream resume across replicas.

The desktop profile keeps cloud features off by design. Features that need the server store say so through
the capability, instead of checking for a nil backend inline.

Routes do the same with `route.Needs(capability.ServerStore, ...)`. When the process lacks a capability the
route needs, the route answers 503 `SERVICE_UNAVAILABLE` before its handler runs, in every `PAD_ROUTE_POLICY`
mode. That is the same answer as the handlers' own "cloud mode required" checks, which move onto their routes
route by route. The router takes its capabilities from `capability.Of(cfg)`. Tests give a router other ones with
`capability.NewSet`, since test routers run on a filesystem backend with the default config.

Running more than one replica is a separate question. The deployment is pinned to one replica
(`infra/main.bicep`) until the realtime backplane exists (Phase 5b of the proposal), even when `shared-state` is
available.
