# DI wiring: constructors, and the setters that remain

The backend is wired with fx in `internal/di` (`services.go`, `api.go`) and `main.go`. A service gets its
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
| `service.AIBudget` (`ai_budget.go`): the daily AI budget rule, today's recorded spend and the ledger of in-flight chat spend | `ChatService.CheckBudget`, which the knowledge base reached through `KnowledgeService.SetBudgetChecker(chat)`. The chat service needs the knowledge base, so the two could not take each other. | `NewChatService` (`ChatOptions.Budget`) and `rag.NewKnowledgeService` (`rag.WithBudgetChecker`), one instance from `di.ProvideAIBudget`. Chat streams enter the ledger when they are admitted and leave it when they end, at the same points as `activeStreams`. |
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
| `ChatService.SetResumeBackplane(redis)` | `ChatOptions.Resume` | No cycle |
| `AnalysisService.SetCustomRules` / `SetRuleProfileResolver` / `SetAuthEnabled` (`decorateAnalysisService`) | `NewAnalysisService(..., AnalysisOptions{...})` via `di.ProvideAnalysisService` | No cycle (the resolver takes the rules, not the service) |
| `factory.SetOrgKeyLookup` (an `fx.Decorate` on `*ai.ProviderFactory`) | Called inside `ProvideAI`, which now takes the `OrgKeyStore` | No cycle. It configures the factory where it is built. |
| `factory.SetEmbeddingDimensions` (in `ProvideAI`) | Unchanged | Configures the object its own provider builds. `ai.NewProviderFactory` has about 65 test call sites. |
| `src.SetAzureExportConfigSource(svc.ExportConnectionConfigs)` (in `ProvideAzureModelService`) | Unchanged; `src` is the database's `storagedb.AzureRegistryProvider` fx value, no longer a type assertion on the backend | **Cycle.** The backend's data-subject export reads Azure connection configs from `AzureModelService`, which is built on the backend. Phase 3's data-lifecycle registry replaces the central export. |
| `AuthHandler.SetProviderService(provider)` (`fx.Decorate`) | `NewAccountLifecycleService(users, provider, bus)` | No cycle: a constructor parameter. |
| `FlowService.SetRuleProfileResolver(resolver)` (`fx.Decorate`) | Unchanged | **Deferred.** No cycle, but `NewFlowService` has about 55 test call sites. |

## What only the database offers: fx values, not type assertions

`main.go`'s `provideStorageBackend` provides the backend and, beside it, each database-only capability as an fx
value of its own, nil without a database (ADR 0010): the cross-replica lock (`lock.GlobalLocker`), the pool for the
token blacklist, the keystore constructor, field encryption, the audit org backfill, the blob probe, the Azure
registry and the request transaction with its Idempotency-Key store (`reqtx.Opener`, `storageif.IdempotencyStore`,
nil without row-level security). A consumer takes the one it needs as a parameter and tests it for nil; nothing
type-asserts the backend to find out (`TestArch_StorageTypeAssertionsRatchet`). Test graphs that build
`ServiceModule` or `APIModule` without `main.go` add `noDatabase()` (`internal/di/wiring_test.go`).

The keystore works the same way: `provideKeyStore` provides the keys (`service.KeyStore`) and, only with the
encrypted database keystore, the organisation credential store (`service.OrgKeyStore`) and the key-family lister
(`storageif.SecretFamilyLister`), nil with the OS keyring. Test graphs add `fakeKeyStore()`.

The roles every backend has are not fx values of their own: the services take the backend through the narrow role
interface in their constructor (refresh tokens and identity links included, adapted in `internal/di/api.go`).

## Wiring outside `internal/di` (not ratcheted)

| Where | Setter | Why it stays |
|---|---|---|
| `main.go` `provideScanner` | `scanner.SetEventNotifier(eventMgr)`, `SetRuleSetDigest(...)` | **Deferred.** No cycle, so these can become `scanner.New` parameters (its cross-replica lock already is one). |
| `api.NewRouter` | `EventManager.SetOriginChecker` / `SetClientKeyFunc` / `SetRevocationChecker` | The router owns the origin, client-key and revocation policy it hands out, so these are wiring inside one component. (The modules' `SetFlowNotifier(hub)` is gone: the `ws-flow-changed` subscriber of `FlowChanged` reaches the hub through `Router.FlowViewers`.) |
| `ProvideAI` | `ai.ConfigureAzureFoundry`, `ai.ConfigureLocalModels` | Process-wide egress settings of the `ai` package, set once at boot |
| `main.go` | `logger.SetPanicHook`, `logger.SetContextAttrs` | Process-wide settings, set once at boot. (The audit spill directory is resolved inside `audit.Provide`.) |

## Test seams

Tests build services with the same constructors. A zero `ChatOptions` gives the chat service a private budget
over its own settings and store. A bare `&ChatService{}` struct literal, common in the service tests, builds
that budget lazily (`aiBudget`). A nil scope resolver passed to `NewLibraryService` builds one over the same
storage and authz.
