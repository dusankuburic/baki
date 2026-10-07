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
| `SystemService.SetAzureModels(azure)` | `NewSystemService(..., azure)` | No cycle |
| `ChatService.SetAzureModels(azure)` | `ChatOptions.Azure` | No cycle |
| `ProviderService.SetAzureModels(azure)` | `NewProviderService(..., orgKeys, azure)` | No cycle |
| `ProviderService.SetOrgKeyStore(orgKeys)` | `NewProviderService(..., orgKeys, azure)` | No cycle |
| `ChatService.SetResumeBackplane(redis)` | `ChatOptions.Resume` | No cycle |
| `AnalysisService.SetCustomRules` / `SetRuleProfileResolver` / `SetAuthEnabled` (`decorateAnalysisService`) | `NewAnalysisService(..., AnalysisOptions{...})` via `di.ProvideAnalysisService` | No cycle (the resolver takes the rules, not the service) |
| `factory.SetOrgKeyLookup` (an `fx.Decorate` on `*ai.ProviderFactory`) | Called inside `ProvideAI`, which now takes the `OrgKeyStore` | No cycle. It configures the factory where it is built. |
| `factory.SetEmbeddingDimensions` (in `ProvideAI`) | Unchanged | Configures the object its own provider builds. `ai.NewProviderFactory` has about 65 test call sites. |
| `src.SetAzureExportConfigSource(svc.ExportConnectionConfigs)` (in `ProvideAzureModelService`) | Unchanged | **Cycle.** The backend's data-subject export reads Azure connection configs from `AzureModelService`, which is built on the backend. Phase 3's data-lifecycle registry replaces the central export. |
| `flowSvc.SetCloudFlowMutationHook(dashboard.InvalidateDashboardCache)` | Unchanged | **Event hook** (Phase 2b, `FlowChanged`). `DashboardService` depends on `FlowService`. |
| `flowSvc.OnInvalidateFlow(chat.InvalidateChatContext)` | Unchanged | **Event hook** (Phase 2b, `FlowChanged`). `ChatService` depends on `FlowService`. |
| `sys.OnApiKeyChanged(provider.InvalidateModels)` | Unchanged | **Event hook** (Phase 2b, `CredentialChanged`) |
| `AuthHandler.OnUserDeleted(provider.InvalidateScope)` (`fx.Decorate`) | `AccountLifecycleService.OnUserDeleted(provider.InvalidateScope)` in its provider | **Event hook** (Phase 2b). Account deletion moved into `AccountLifecycleService`. |
| `AuthHandler.SetProviderService(provider)` (`fx.Decorate`) | `NewAccountLifecycleService(users, provider)` | No cycle: a constructor parameter. |
| `FlowService.SetRuleProfileResolver(resolver)` (`fx.Decorate`) | Unchanged | **Deferred.** No cycle, but `NewFlowService` has about 55 test call sites. |

## Wiring outside `internal/di` (not ratcheted)

| Where | Setter | Why it stays |
|---|---|---|
| `main.go` `provideScanner` | `scanner.SetEventNotifier(eventMgr)`, `SetRuleSetDigest(...)` | **Deferred.** No cycle, so these can become `scanner.New` parameters. |
| `api.NewRouter` | `SetFlowNotifier(hub)` on modules, `EventManager.SetOriginChecker` / `SetClientKeyFunc` / `SetRevocationChecker` | The router builds the hub and owns the origin, client-key and revocation policy it hands out, so these are wiring inside one component. |
| `ProvideAI` | `ai.ConfigureAzureFoundry`, `ai.ConfigureLocalModels` | Process-wide egress settings of the `ai` package, set once at boot |
| `main.go` | `logger.SetPanicHook`, `logger.SetContextAttrs` | Process-wide settings, set once at boot. (The audit spill directory is resolved inside `audit.Provide`.) |

## Test seams

Tests build services with the same constructors. A zero `ChatOptions` gives the chat service a private budget
over its own settings and store. A bare `&ChatService{}` struct literal, common in the service tests, builds
that budget lazily (`aiBudget`). A nil scope resolver passed to `NewLibraryService` builds one over the same
storage and authz.
