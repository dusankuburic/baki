# Test safety nets

The goldens and ratchets that make a behaviour change deliberate: it shows up as a reviewable diff. Refresh a golden only for an intended change and review the diff — dashboards, alerts and clients read these. Golden tests compare with CR stripped, and update mode deletes a golden whose input is gone.

## Goldens and their refresh flags

`go test <package> -run <Test> <flag>`, e.g. `go test ./internal/api/ -run TestRoutePolicy_Golden -update-route-policy`:

| Golden | Covers | Refresh flag |
|---|---|---|
| `internal/api/testdata/route_policy.golden` | the legacy tables' decision (route, auth, minimal scopes, bucket, RLS, deadline, label) for every registered route in bypass-prone spellings | `-update-route-policy` |
| `internal/api/testdata/route_policy_declared.golden` | the declared policy per request; 405/OPTIONS/no-route lines differ from legacy by design and are reviewed here | `-update-route-policy-declared` |
| `internal/api/testdata/authz_matrix.golden` | every route group × anonymous/viewer/owner/org outsider/admin/scoped tokens | `-update-authz-matrix` |
| `internal/storage/database/testdata/migration_checksums.golden` | every shipped step's checksum (an edited migration fails CI, not boot) | `-update-migration-checksums` |
| `core/analyzer/testdata/findings/*.golden` | the whole sample corpus × six settings variants: findings, order, fingerprints, health | `-update-findings` |
| `core/export/testdata/golden/*.golden` | every registered format × five reports (PDF as SHA-256 of the bytes) | `-update-export-golden` |
| `internal/mail/testdata/*.golden` | every mail template: subject, bodies, whole MIME message (fixtures carry forged boundaries) | `-update-mail-golden` |
| `internal/notify/testdata/payloads/*.golden` | each event type through each channel kind: request line, headers, body bytes | `-update-notify-golden` |
| `internal/di/testdata/caches.golden` | every declared cache: key parts, tenant, locality | `-update-caches` |
| `internal/di/testdata/subscribers.golden` | every event subscriber and its locality (below) | `-update-subscribers` |
| `internal/lifecycle/testdata/jobs.golden` | every job, its advisory-lock key and schedule | `-update-jobs` |
| `internal/metrics/testdata/metrics.golden` | every metric family with its owner | `-update-metrics` |
| `internal/ai/testdata/providers.golden` | the provider spec registry | `-update-provider-specs` |
| `internal/ai/testdata/tools.golden` | the chat tool specs | `-update-tools` |

`TestRoutePolicy_DeclaredMatchesLegacy` fails when a routed request decides anything differently from the legacy tables. The API suite runs under the declared policy with `BAKI_TEST_ROUTE_POLICY=declarative go test ./internal/api/...`; at boot `finishRoutes` resolves a sample path of every route and refuses to start unless each resolves to its own policy.

## Event subscribers: `internal/di/testdata/subscribers.golden`

Every subscriber must declare, as a required option of `eventbus.Subscribe`, whether the other replicas must reproduce its effect. There is no default:

- `eventbus.Local(reason)` — the effect stays on this replica: a side effect that has to happen once (an email, a bell row), or a cache that cannot go stale across replicas.
- `eventbus.Replicated(invalidations, keys...)` — the other replicas reproduce it. Keys name declared caches or `recheck.*` nudges; the subscriber drops only what it derives from the event, idempotently, from fields that are not secret and not free text.

Enforcement: an unclassified subscriber fails `TestSubscribers_EveryOneChoseItsLocality`; every `eventbus.Subscribe` call must sit in a registrar the golden drives (`TestSubscribers_EverySubscribeCallIsExercisedByTheGolden`); keys are checked against declared caches or rechecks; every replicated key must register an applier beside itself (`Bus.OnRemote`; `TestSubscribers_EveryReplicatedKeyHasAnApplier`); replicated caches must be declared `cache.Shared` and shared caches replicated (`TestSubscribers_ReplicatedCachesAreSharedAndSharedCachesAreReplicated`). Replicated invalidations travel only after the publisher's commit (through `txhook` and the `eventbus.Replicas` hook), arrive as `Bus.DeliverRemote(key, scope)` — which runs the applier (same generation bump and delete as the local subscriber) and never re-publishes — and `Bus.Invalidate` announces a change that is not a domain event (same rules). Scope vocabulary (the subscriber writes it, the applier reads it): `settings.deployment`, `rules.deployment-profile` (whole row); `rules.profiles` (none / `org` / `org`+`user`); `providers.model-listings` (`scope`, plus `provider`); `rag.corpus-counts` (`org`); `dashboard.home`, `dashboard.analytics` (`owner` and/or `org`); `recheck.ws-access` (`what`+`id`, or `credential` with `user`+`jti`); `recheck.sse-revocation` (`jti`).

## Ratchets and pinned lists (`internal/archtest`)

Pinned exceptions fail when they grow and when they shrink, so the numbers only go down: layering and store-handle pins; `TestArch_SystemReadsPinned` (the jobs allowed to read across tenants, ADR 0013); raw cache constructions and `singleflight.Group`s per file; `TestArch_PerProcessStateDeclared` — every keyed per-process state is an entry of `stateRegistry` with a locality (replica-local with a reason, shared with a mechanism and a proving test, database, request-scoped, config, desktop- or test-only; ADR 0032); `TestArch_ConfigNotMutatedOutsideConfig` (go/types: no write to anything reached from a `config.Config` outside `internal/config`); provider-id literals outside the specs per file; `SaveUser` outside storage and tests; `TestRLS_TablesWithoutRowSecurityAreListed` (a catalog query, runs in CI; a new table needs a policy or a listed reason); `TestReseal_CoversEverySealedColumn` (every `sealField` column in the rotation re-seal pass); `TestDISetters_Listed`; `TestArch_RoutesUseTheTypedAdapter`.

## Fakes and isolated databases

The storage contract (`internal/storage/contract`) also runs against `testutil.FakeBackend` with no known divergences (`CONTRACT_STRICT=1`), including the data-lifecycle scenarios (`contract/lifecycle.go`) — a changed step changes the fake and the scenario with it. The fake refuses rows whose parent is missing where Postgres has a foreign key (collaborators, versions, comments, invitations, settings, and since migration 0048 sessions, policies, channels, custom rules, the knowledge base): seed the user, org or flow first. The legacy source contract (`contract/legacy.go`) writes fixtures as raw files and proves the read-only legacy source never creates, changes or removes anything. Postgres behaviour itself (RLS, transactions, statement counts) is tested through `internal/testutil/pgtest`: databases cloned from a template migrated once per schema version; the role needs `CREATEDB` (CI grants it) and a `baki_template` database carrying the `vector` extension; it refuses a database named `baki` — never point it at the live compose database. Plan tests (EXPLAIN) must open a database of their own, seed in random physical order and `ANALYZE` before `EXPLAIN`.

## CI jobs and boot tests

- `previous-release-boot` (`scripts/ci/previous-release-boot.sh`): the previous release boots on the new schema, then this revision (the deploy), then the previous again (the rollback) — every newer step must be `-- baki:compat expand`; a `breaking` step fails the job and the deploy it gates (ship one via a manual `deploy.yml` run, which has no previous commit).
- `two-replica` (`docker-compose.replicas.yml`, `scripts/ci/two-replica-smoke.sh`): the gate for raising `maxReplicas`; `internal/di/two_replicas_test.go` runs the cross-replica scenarios in process, and `deploy_replicas_test.go` keeps the repository shipping one replica without Redis.
- `TestAppGraph` (`fx.ValidateApp`) checks every provider and invoke has its dependencies; `TestApp_StopOrder` boots the real graph and pins the stop hooks (see [di-wiring.md](di-wiring.md#the-application-internalapp)); `TestDesktopBoot_Smoke` runs the binary as the Tauri shell does (`CONFIG:` first line, `/healthz`, clean SIGTERM stop within the budget).
- `BenchmarkBuildHandler` sends one request through the production stack over the fake; the nightly `chain-benchmark` job benchstats it against the previous day.
