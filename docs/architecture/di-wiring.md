# DI wiring: constructors, and the setters that remain

The backend is wired with fx in `internal/di` (`services.go`, `api.go`) and `internal/app`, the application whose
graph `main.go` runs (see [The application](#the-application-internalapp) below). A service gets its
dependencies through its constructor. A post-construction setter (`SetX`, `OnX`) is allowed only for one of
three reasons:

- **Cycle.** The two components need each other, and no extracted component breaks the cycle yet.
- **Event hook.** One component tells another that its data changed so a cache can drop. Phase 2b's domain
  event bus replaces these hooks with subscriptions made in `fx.Invoke`.
- **Deferred.** There is no cycle, but moving the dependency into the constructor touches many test call sites.
  These setters are listed so they get converted when that code is next touched.

`internal/di/setters_test.go` (`TestDISetters_Listed`) lists every `Set*`/`On*` call in `internal/di` with its
reason. A new setter fails the test until it is listed there (and documented on this page), and a removed one
fails until its entry goes. Prefer a constructor parameter. When two services need each other, extract the part
both need instead.

## Components extracted to break cycles (Phase 2a)

| Component | Was | Taken by |
|---|---|---|
| `service.AIBudget` (`ai_budget.go`): the daily AI budget rule, today's recorded spend and the ledger of in-flight chat spend | `ChatService.CheckBudget`, which the knowledge base reached through `KnowledgeService.SetBudgetChecker(chat)`. The chat service needs the knowledge base, so the two could not take each other. | `NewChatService` (`ChatOptions.Budget`), `rag.NewKnowledgeService` (`rag.WithBudgetChecker`) and, as the `ai.BudgetChecker` every provider chain consults before a billed call (ADR 0028), `ai.NewProviderFactory` (`ai.WithBudgetChecker`, passed by `ProvideAI`); one instance from `di.ProvideAIBudget`, which depends on settings and the backend alone. `service.ProviderPolicy` (`ProvideProviderPolicy`, the same two inputs) reaches the factory the same way (`ai.WithProviderPolicy`). Chat streams enter the ledger when they are admitted and leave it when they end, at the same points as `activeStreams`. |
| `service.FlowScopeResolver` (`flow_scope.go`): what a library scope covers for a caller (owned, shared, org membership) as a storage filter, and the scope's flow ids | `LibraryService.buildLibraryFilter`/`ScopeFlowIDs`, which the analytics page reached through `DashboardService.SetScopeResolver(lib)` | `NewLibraryService` and `NewDashboardService` (as `ScopeResolver`), one instance from fx |

Azure model access needed no extraction. `AzureModelService` depends on none of the services that took it
through `SetAzureModels`, so it became a constructor parameter.

## Every setter, before and after

| Setter (before) | After | Why |
|---|---|---|
| `knowledge.SetBudgetChecker(chat)` (`wireKnowledgeBudget`) | `rag.WithBudgetChecker(budget)` in `ProvideKnowledgeService` | Cycle broken by `AIBudget` |
| `ChatService.SetKnowledgeService(knowledge)` | `ChatOptions.Knowledge` | No cycle once the budget is extracted |
| `DashboardService.SetScopeResolver(lib)` | `NewDashboardService(..., scopes)` | `FlowScopeResolver` extracted, so both services take it |
| `SystemService.SetAzureModels(azure)` | `NewSystemService(..., azure, bus)` | No cycle |
| `ChatService.SetAzureModels(azure)` | `ChatOptions.Azure` | No cycle |
| `ProviderService.SetAzureModels(azure)` | `NewProviderService(..., orgKeys, azure)` | No cycle |
| `ProviderService.SetOrgKeyStore(orgKeys)` | `NewProviderService(..., orgKeys, azure)` | No cycle |
| `ChatService.SetResumeBackplane(redis)` | `ChatOptions.Resume` (+ `ChatOptions.Instance`; the provider also starts the replica's command subscriptions in an OnStart hook, `Shutdown` ends them) | No cycle |
| `AnalysisService.SetCustomRules` / `SetRuleProfileResolver` / `SetAuthEnabled` (`decorateAnalysisService`) | `NewAnalysisService(..., AnalysisOptions{...})` via `di.ProvideAnalysisService` | No cycle (the resolver takes the rules, not the service) |
| `factory.SetOrgKeyLookup` (an `fx.Decorate` on `*ai.ProviderFactory`) | Called inside `ProvideAI`, which now takes the `OrgKeyStore` | No cycle. It configures the factory where it is built. |
| `factory.SetEmbeddingDimensions` (in `ProvideAI`) | Unchanged | Configures the object its own provider builds. `ai.NewProviderFactory` has about 65 test call sites. |
| `src.SetAzureExportConfigSource(svc.ExportConnectionConfigs)` (in `ProvideAzureModelService`) | Unchanged; `src` is the database's `storagedb.AzureRegistryProvider` fx value, no longer a type assertion on the backend | **Cycle.** The backend's data-subject export reads Azure connection configs from `AzureModelService`, which is built on the backend. Phase 3's data-lifecycle registry replaces the central export. |
| `AuthHandler.SetProviderService(provider)` (`fx.Decorate`) | `NewAccountLifecycleService(users, provider, bus)` | No cycle: a constructor parameter. |
| `FlowService.SetRuleProfileResolver(resolver)` (`fx.Decorate`) | Unchanged | **Deferred.** No cycle, but `NewFlowService` has about 55 test call sites. |

## What only the database offers: fx values, not type assertions

`internal/app`'s `provideStorageBackend` (`storage.go`) provides the backend and, beside it, each database-only
capability as an fx value of its own, nil without a database (ADR 0010): the cross-replica lock (`lock.GlobalLocker`), the pool for the
token blacklist, the keystore constructor, field encryption, the audit org backfill, the blob probe, the Azure
registry and the request transaction with its Idempotency-Key store (`reqtx.Opener`, `storageif.IdempotencyStore`,
nil without row-level security). A consumer takes the one it needs as a parameter and tests it for nil; nothing
type-asserts the backend to find out (`TestArch_StorageTypeAssertionsRatchet`). Test graphs that build
`ServiceModule` or `APIModule` without `internal/app` add `noDatabase()` (`internal/di/wiring_test.go`).

The keystore works the same way: `provideKeyStore` (`internal/app/keystore.go`) provides the keys (`service.KeyStore`) and, only with the
encrypted database keystore, the organisation credential store (`service.OrgKeyStore`), the key-family lister
(`storageif.SecretFamilyLister`) and the key-rotation re-seal pass (`storagedb.KeyResealer`), nil with the OS keyring.
Test graphs add `fakeKeyStore()`.

The roles every backend has are not fx values of their own: the services take the backend through the narrow role
interface in their constructor (refresh tokens and identity links included, adapted in `internal/di/api.go`).

## Wiring outside `internal/di` (not ratcheted)

| Where | Setter | Why it stays |
|---|---|---|
| `internal/app` `provideScanner` (`governance.go`) | `scanner.SetEventNotifier(eventMgr)`, `SetRuleSetDigest(...)`, `SetSingleTenant`, `SetHealthRecorder` | **Deferred.** No cycle, so these can become `scanner.New` parameters (its cross-replica lock already is one). |
| `api.NewRouter` | `EventManager.SetOriginChecker` / `SetClientKeyFunc` / `SetRevocationChecker` | The router owns the origin, client-key and revocation policy it hands out, so these are wiring inside one component. (The modules' `SetFlowNotifier(hub)` is gone: the `ws-flow-changed` subscriber of `FlowChanged` reaches the hub through `Router.FlowViewers`, and the `ws-access-recheck` subscribers, which re-check open sockets on an access change (ADR 0020), through `Router.WebSocketHub`.) |
| `ProvideAI` | `ai.ConfigureAzureFoundry`, `ai.ConfigureLocalModels` | Process-wide egress settings of the `ai` package, set once at boot |
| `internal/app` `initLogger` (`observability.go`) | `lifecycle.SetInstanceID`, `errreport.SetInstanceID`, `logger.SetPanicHook`, `logger.SetContextAttrs` | Process-wide settings, set once at boot. (The audit spill directory is resolved inside `audit.Provide`.) |

## The application: `internal/app`

`main.go` keeps what must stay in package `main`: the swag general-API annotations (`swag init -g main.go`), the
ldflags stamps `Version`, `GitCommit` and `BuildDate` (`buildstamp_test.go`), and `main()`, which runs the
`healthcheck` subcommand or `fx.New(app.Options(app.Build{Version, Commit})...).Run()`. It is the package's only
non-test file, because the Dockerfile and CI build it as `go build main.go` (`TestMainPackageIsMainGoAlone`).
Everything else is `internal/app`, one file per concern:

| File | Holds |
|---|---|
| `app.go` | `Build`, `Options` (the stop timeout, the providers, `di.ServiceModule`, `di.APIModule`, `redisx.Module` (the optional `*redis.Client` and the `*redisx.Health` that watches it: a boot ping that does not refuse, a 5 s ping loop on the lifecycle; `di.ProvideRedisPinger` hands the health to the system handler as `api.RedisPinger`, nil without Redis), `invalidation.Module` (with a Redis client: installs the fan-out as the bus's `Replicas` hook and runs it; invoked after the modules whose subscribers register the appliers it delivers to, stopped after the HTTP server so a draining replica's last writes are still sent), the ordered Invoke list), `initAuditTrail` |
| `config.go` | `loadConfig`, the provider of `config.Load` (`PAD_CONFIG`, else the `PAD_*` environment, else the defaults; durations parsed, everything validated, secrets, aliases and the instance id resolved once) |
| `observability.go` | `initLogger`, `initTelemetry` |
| `storage.go` | `StorageResult`, `provideStorageBackend`, `storageConfig` (the `PAD_DB_*` pool settings, the pool check's replica count and server cap, the `application_name`, blob), the pool-metrics loop and the background HNSW build |
| `keystore.go` | `keystoreKey`, `keystorePreviousKeys`, `KeyStoreResult`, `provideKeyStore`, `initFieldEncryption` |
| `auth.go` | `provideAuthManager` (the token blacklist and its cache), `provideOrgService` |
| `governance.go` | `provideNotifier` and `mailSvcAdapter`, `provideScanner`, `initScanner`, `provideMigrationRunner`, `provideScanNowFunc` |
| `jobs_retention.go` | `initRetentionPurge`, `retentionLockKey`, `runRetentionSweep` |
| `jobs_backfill.go` | `fieldEncryptionLockKey`, `scheduleFieldEncryptionBackfill` (the audit backfill is `audit.ScheduleOrgBackfill`) |
| `jobs_reseal.go` | `scheduleSecretReseal`, `runSecretReseal`, `secretResealLockKey` (ADR 0005) |
| `server.go` | `provideShutdownCh`, `startServer` (the listener and the desktop `CONFIG:` handshake at Invoke time, the OnStop sequence), `warnIfMetricsExposed`, `writeSessionSecret` |

**The order is load-bearing.** fx runs the stop hooks in reverse registration order, and a hook registers when the
function that appends it runs. fx runs the invokes of a module (`fx.Module`) before those of the application's own
list, which run in list order. The `observability` module (`observability.go`) is the one module: `initLogger` names
the replica (`lifecycle.SetInstanceID` for the job state, `errreport.SetInstanceID`) and installs the JSON logger,
and `initTelemetry` registers its hook, before anything else is built. Next come the invokes `di.ServiceModule` and
`di.APIModule` carry (their event subscriptions): they construct the storage (the migrations), the audit recorder,
the auth manager and the router. Then `Options`' Invoke list: `initAuditTrail`, `initRetentionPurge`,
`initFieldEncryption`, `startServer`, and after it the jobs whose stop hooks must run before the server drains the
background runners (`initScanner`, `audit.ScheduleOrgBackfill`, `scheduleFieldEncryptionBackfill`,
`scheduleSecretReseal`). In the cloud the hooks stop in this order:

1. the re-seal pass, the field-encryption backfill and the audit backfill are cancelled;
2. the scanner stops;
3. the server: SSE closed, chat streams cancelled, WebSockets drained, `http.Server.Shutdown`, `lifecycle.DrainAll`;
4. the retention loop stops;
5. the router's WebSocket hub, the auth manager's blacklist cache, the audit recorder (it drains), the storage (the
   pool closes);
6. telemetry flushes.

`TestApp_StopOrder` (`internal/app`) pins this list and the desktop one. Because the observability module runs
first, every log line comes from the configured logger, JSON with `instance_id` (in the cloud on stdout, the
migration lines included: `TestApp_LogsAreJSONFromTheFirstLine`), and telemetry flushes after the audit drain and
the pool close. Only `config.Load`, which the logger needs, runs before it; it does not log.

A wave-2 change goes to the file of its concern: the typed configuration to `config.go` and `storage.go`, a job on
`lifecycle.Singleton` to the `jobs_*.go` file and `governance.go`, the instance id to `observability.go`, the drain
sequence to `server.go` (and its hook to the end of the Invoke list in `app.go`, which is otherwise frozen).

## Test seams

Tests build services with the same constructors. A zero `ChatOptions` gives the chat service a private budget
over its own settings and store. A bare `&ChatService{}` struct literal, common in the service tests, builds
that budget lazily (`aiBudget`). A nil scope resolver passed to `NewLibraryService` builds one over the same
storage and authz.
