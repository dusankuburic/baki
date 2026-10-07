package di

import (
	"context"
	"errors"
	"fmt"
	"pad-core/logger"
	"path/filepath"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-core/models"
)

func ProvideDocumentProvider(mode config.DeploymentMode, storageBackend storageif.StorageBackend) service.DocumentProvider {
	if mode == config.ModeLocal {
		return service.NewLocalDocumentProvider()
	}
	return service.NewCloudDocumentProvider(storageBackend)
}

// ProvideSettingsStore builds the desktop app-settings store, backed by a file
// under the user's config directory. Cloud mode gets a memory-only instance:
// nothing there should be reading it (see ProvideSettingsProvider) and a
// file-backed store would force a writable home directory.
func ProvideSettingsStore(mode config.DeploymentMode) (*storage.SettingsStore, error) {
	if mode == config.ModeCloud {
		return storage.NewInMemorySettingsStore(), nil
	}
	return storage.NewSettingsStore()
}

// ProvideSettingsProvider selects where deployment-wide settings live.
//
// Cloud mode reads and writes them through the storage backend (Postgres
// app_settings). It previously used the same local store as the desktop, which
// split deployment settings in two: writes went to the database while the
// analysis path read the local file — recreated from defaults on every
// container start — so a saved configuration had no effect on analysis at all.
func ProvideSettingsProvider(
	ctx context.Context,
	mode config.DeploymentMode,
	store *storage.SettingsStore,
	backend storageif.StorageBackend,
) service.SettingsProvider {
	if mode == config.ModeCloud && backend != nil {
		return service.NewBackendSettingsProvider(ctx, backend)
	}
	return store
}

// ProvideConfigDir resolves the per-user application directory — a desktop
// concept: in cloud mode the container's filesystem is ephemeral and (under
// the compose production overlay) read-only, so this returns an empty path
// and the stores that take it degrade to no-ops. Returning a path here also
// made boot depend on $HOME being set inside a distroless image.
func ProvideConfigDir(mode config.DeploymentMode) (string, error) {
	if mode == config.ModeCloud {
		return "", nil
	}
	return storage.ConfigDir()
}

// astCacheBudget bounds the heap the parsed-document (AST) cache pins: the
// desktop's "reopen the same file" shortcut, keyed by content hash. An
// entry-count LRU would let a few large flows pin gigabytes alive at ~1 KiB
// per block (service.FlowDocumentCost); the byte budget keeps a working set of
// recent files. In cloud mode FlowService keeps no AST cache (NewFlowService).
const astCacheBudget int64 = 96 << 20

// ProvideASTCache builds the parsed-document cache, charged per document by
// the memory-audit cost model (blocks × 1 KiB + the source it carries).
func ProvideASTCache() (cache.Cache, error) {
	return cache.NewSizedLRU(astCacheBudget, cache.WithCostFunc(astEntryCost)), nil
}

// astEntryCost is the AST cache's cost function: a document's estimated heap,
// or a nominal 1 for anything else stored in it.
func astEntryCost(value any) int64 {
	if doc, ok := value.(*models.FlowDocument); ok {
		return service.FlowDocumentCost(doc)
	}
	return 1
}

// ProvideHistoryStore persists per-flow analysis trend snapshots under the
// app config dir (HistoryStore treats an empty dir as a no-op). An empty
// configDir must stay empty rather than becoming the relative path
// "analysis-history", which would write into the container's working directory.
// Cloud mode keeps durable analysis history in Postgres.
func ProvideHistoryStore(configDir string) *analyzer.HistoryStore {
	if configDir == "" {
		return analyzer.NewHistoryStore("")
	}
	return analyzer.NewHistoryStore(filepath.Join(configDir, "analysis-history"))
}

// ProvideAI builds the GitHub/Copilot auth and the provider factory. With an organisation credential store
// (cloud) ForOrg and ForEmbeddingOrg fall back to — or, for embeddings, prefer — the org's shared credential.
func ProvideAI(configDir string, backend storageif.StorageBackend, cfg *config.Config, keys service.KeyStore, orgKeys service.OrgKeyStore) (*ai.GitHubAuth, *ai.CopilotAuth, *ai.ProviderFactory) {
	copilotAuth := ai.NewCopilotAuth()
	// The storage backend is nil in local/desktop mode (no usage store). Leave the
	// recorder nil there — the audited provider guards a nil recorder and skips
	// recording — rather than a closure that would dereference the nil backend
	// and panic in the goroutine record() spawns after every completion.
	var recorder ai.UsageRecorder
	if backend != nil {
		recorder = func(ctx context.Context, metric *storageif.UsageMetric) error {
			return backend.SaveUsageMetric(ctx, metric)
		}
	}
	factory := ai.NewProviderFactory(memberKeyLookup(keys), copilotAuth, recorder, &cfg.Runtime)
	if orgKeys != nil {
		factory.SetOrgKeyLookup(orgKeyLookup(orgKeys))
	}
	// Embeddings come back at the knowledge base's vector width where the
	// model supports it, so they are indexable. The width is the storage
	// contract's, default included: passing the raw PAD_EMBEDDING_DIM (0 when
	// unset) asked for nothing, and text-embedding-3-large then returned
	// 3072-wide vectors against the 1536 index.
	factory.SetEmbeddingDimensions(cfg.Storage.EffectiveEmbeddingDim())
	// Azure AI Foundry endpoints are user-supplied. In cloud mode every
	// request to one goes through the dial-time private-address guard (the
	// operator can lift it for private-link resources); the desktop/local
	// single-user app talks to its owner's own network and needs none.
	policy := egress.AllowLoopback
	if cfg.Mode == config.ModeCloud {
		policy = egress.BlockInternal
	}
	ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{
		Egress:             policy,
		ExtraAllowedHosts:  cfg.Runtime.AzureFoundryAllowedHosts,
		AllowPrivateEgress: cfg.Runtime.AIAllowPrivateEgress,
	})
	// The local models provider (Foundry Local, Ollama, LM Studio) is
	// constructible only in the desktop/local single-user app, whose loopback
	// allowance is exactly the deliberate opt-in it needs.
	ai.ConfigureLocalModels(ai.LocalModelsOptions{Egress: policy})
	auth := ai.NewGitHubAuth()
	return auth, copilotAuth, factory
}

// memberKeyLookup adapts a KeyStore to the factory's contract, under which
// "no key stored" is ("", nil) and only a real failure (a decrypt failure
// after a secret rotation, a database error) is an error. Both keystores
// report absence as storage.ErrSecretNotFound; passed through unmapped it
// made a plainly unconfigured provider look broken (chat showed
// "couldn't load the AI provider configuration" instead of
// PROVIDER_NOT_CONFIGURED) and kept ForOrg from ever reaching the
// organisation's shared credential.
func memberKeyLookup(keys service.KeyStore) func(scope, provider string) (string, error) {
	return func(scope, provider string) (string, error) {
		v, err := keys.Get(scope, provider)
		if errors.Is(err, storage.ErrSecretNotFound) {
			return "", nil
		}
		return v, err
	}
}

// orgKeyLookup is memberKeyLookup for an organisation's shared credentials.
func orgKeyLookup(keys service.OrgKeyStore) func(orgID, provider string) (string, error) {
	return func(orgID, provider string) (string, error) {
		v, err := keys.GetOrg(orgID, provider)
		if errors.Is(err, storage.ErrSecretNotFound) {
			return "", nil
		}
		return v, err
	}
}

// ProvideOrgKeyStore exposes the organisation credential store (G7) when the
// deployment's keystore can hold one — the encrypted database keystore of
// cloud mode implements both interfaces; the OS keyring does not — and nil
// otherwise (desktop, local mode), which the org routes report as
// ORG_KEYSTORE_UNAVAILABLE.
func ProvideOrgKeyStore(keys service.KeyStore) service.OrgKeyStore {
	if store, ok := keys.(service.OrgKeyStore); ok {
		return store
	}
	return nil
}

// ProvideAnalysisService builds the AnalysisService on the deployment's custom rules and the per-org rule
// resolver (the resolver takes the same compiled slice, ProvideDeploymentCustomRules).
func ProvideAnalysisService(notifier service.EventNotifier, settings service.SettingsProvider, history *analyzer.HistoryStore, cfg *config.Config, rules service.DeploymentCustomRules, resolver *service.RuleProfileResolver) (*service.AnalysisService, error) {
	return service.NewAnalysisService(notifier, settings, history, service.AnalysisOptions{
		CustomRules: rules,
		Profiles:    resolver,
		// With auth on, an analysis that carries no user (the scanner) must not
		// broadcast progress to every connected client.
		AuthEnabled: cfg.Auth.Enabled,
	})
}

// ProvideAIBudget builds the daily AI budget gate chat turns and the knowledge base's billed embedding runs
// share (R-4): one in-flight ledger, so an embedding run counts what running chat streams have committed.
// backend is nil on the desktop (no usage store: spend reads $0).
func ProvideAIBudget(settings service.SettingsProvider, backend storageif.StorageBackend) *service.AIBudget {
	return service.NewAIBudget(settings, backend)
}

// ProvideKnowledgeService builds the org knowledge-base service.
//
// It takes the factory (not a pre-resolved provider) so the embedding
// provider is resolved per request in the caller's scope — resolving once
// here with an empty scope fails in cloud mode (keys are per-user) and never
// picks up keys added later. settings is the service.SettingsProvider, NOT
// the concrete *storage.SettingsStore: in cloud mode the configured embedding
// provider and model live in the database, and taking the concrete store read
// them from a local file that always held the defaults.
// PAD_EMBEDDING_PROVIDER (validated at boot) pins the embedding provider for
// the deployment; it is the only way the knowledge base embeds with Azure AI
// Foundry — through the model registry's "Use for knowledge base"
// designation when azure (the registry) is wired. budget is the daily AI budget
// a chat turn consults too (R-4); nil leaves billed runs unlimited.
func ProvideKnowledgeService(backend storageif.StorageBackend, factory *ai.ProviderFactory, settings service.SettingsProvider, cfg *config.Config, azure *service.AzureModelService, budget *service.AIBudget) *rag.KnowledgeService {
	opts := []rag.Option{rag.WithEmbeddingProvider(cfg.Storage.EmbeddingProvider)}
	if azure != nil {
		opts = append(opts, rag.WithAzureEmbedder(azure))
	}
	if budget != nil {
		opts = append(opts, rag.WithBudgetChecker(budget))
	}
	return rag.NewKnowledgeService(backend, factory, settings, opts...)
}

// ProvideAzureRegistry selects where the Azure AI Foundry model registry
// lives: Postgres when authentication is on and the backend is the database
// one — only then are owner ids real users and organisations and the foreign
// keys (and their cascades) hold — else the per-user config directory's
// azure-models.json (the desktop, and a database backend with auth off),
// whose one personal registry is owner "". Never settings.json. orgKeys only
// decides whether organisations can hold registries (logged). Without a
// config directory the registry is kept in memory, with a warning.
func ProvideAzureRegistry(cfg *config.Config, backend storageif.StorageBackend, orgKeys service.OrgKeyStore, configDir string) (storageif.AzureRegistryStore, error) {
	if cfg.Auth.Enabled && backend != nil {
		if p, ok := backend.(storagedb.AzureRegistryProvider); ok {
			logger.Info("azure registry: database store", "organisationRegistries", orgKeys != nil)
			return p.AzureRegistry(), nil
		}
	}
	if configDir == "" {
		logger.Warn("azure registry: no config directory and no database registry; Azure AI Foundry resources and models are kept in memory only")
		return storage.NewMemoryAzureRegistry(), nil
	}
	reg, err := storage.NewFileAzureRegistry(configDir)
	if err != nil {
		return nil, fmt.Errorf("azure registry: %w", err)
	}
	return reg, nil
}

// ProvideAzureModelService builds the Azure AI Foundry model registry service.
// Per-user and organisation registries exist with the database store; the
// backend (nil on the desktop) supplies the caller's settings, organisation
// membership, the knowledge corpus counts and user names. The knowledge-base
// designation is offered where the knowledge base embeds with Azure (cloud
// with PAD_EMBEDDING_PROVIDER=azure-foundry); the data-subject export reads
// each personal resource's public configuration through it.
func ProvideAzureModelService(registry storageif.AzureRegistryStore, secrets service.KeyStore, orgKeys service.OrgKeyStore, factory *ai.ProviderFactory, settings service.SettingsProvider, backend storageif.StorageBackend, cfg *config.Config) *service.AzureModelService {
	_, perUser := registry.(*storagedb.PostgresAzureRegistry)
	deps := service.AzureModelServiceDeps{
		Registry:                  registry,
		Secrets:                   secrets,
		OrgKeys:                   orgKeys,
		Factory:                   factory,
		Settings:                  settings,
		PerUserOwners:             perUser,
		KnowledgeEmbeddingEnabled: cfg.Mode == config.ModeCloud && cfg.Storage.EmbeddingProvider == ai.AzureFoundryProviderID,
	}
	if backend != nil {
		deps.UserSettings, deps.Orgs, deps.Corpus, deps.Users = backend, backend, backend, backend
	}
	svc := service.NewAzureModelService(deps)
	if perUser {
		if src, ok := backend.(interface {
			SetAzureExportConfigSource(storageif.AzureExportConfigFunc)
		}); ok {
			src.SetAzureExportConfigSource(svc.ExportConnectionConfigs)
		}
	}
	return svc
}

// The org-governance services take the role interfaces they use, which fx
// cannot derive from the StorageBackend it provides, hence these adapters. On
// the desktop the backend is nil and so are their stores; the routes that
// reach them declare the server store.

func ProvidePolicyService(backend storageif.StorageBackend) *service.PolicyService {
	return service.NewPolicyService(backend)
}

func ProvideGovernanceAlertService(backend storageif.StorageBackend) *service.GovernanceAlertService {
	return service.NewGovernanceAlertService(backend)
}

func ProvideOrgRuleService(backend storageif.StorageBackend, profiles *service.RuleProfileResolver) *service.OrgRuleService {
	return service.NewOrgRuleService(backend, backend, profiles)
}

func ProvideChannelService(backend storageif.StorageBackend) *service.ChannelService {
	return service.NewChannelService(backend)
}

func ProvideUserDirectory(backend storageif.StorageBackend) *service.UserDirectory {
	return service.NewUserDirectory(backend)
}

// ProvideTriageService, ProvideCommentService and ProvideShareTokenService
// narrow the full backend to the role interfaces each service takes (fx only
// provides storageif.StorageBackend). On the desktop the backend is nil and so
// are the roles; the routes that reach these services need the server-side
// store.
func ProvideTriageService(backend storageif.StorageBackend) *service.TriageService {
	return service.NewTriageService(backend, backend, backend)
}

func ProvideCommentService(backend storageif.StorageBackend, triage *service.TriageService) *service.CommentService {
	return service.NewCommentService(backend, triage)
}

func ProvideShareTokenService(backend storageif.StorageBackend) *service.ShareTokenService {
	return service.NewShareTokenService(backend, backend)
}

// ServiceModule bundles all internal services.
var ServiceModule = fx.Options(
	fx.Provide(
		ProvideDocumentProvider,
		ProvideSettingsStore,
		ProvideConfigDir,
		ProvideASTCache,
		ProvideHistoryStore,
		ProvideAI,
		ProvideSettingsProvider,
		// WebhookNotifier is a thin facade over the shared notify.Dispatcher,
		// consolidating analysis-complete alerts onto the same delivery stack as
		// governance (scanner) alerts; provided by main.go's provideNotifier.
		service.NewWebhookNotifier,
		// service.KeyStore itself comes from main.go's provideKeyStore (a plain
		// fx provider, not a global) — no adapter needed here.
		ProvideOrgKeyStore,
		ProvideAzureRegistry,
		ProvideAzureModelService,
		ProvideAIBudget,
		ProvideKnowledgeService,
		// /api/keys/has and /api/keys/delete for azure-foundry act on the
		// model registry (any resource; resource "default").
		service.NewSystemService,
		service.NewAuthzService,
		service.NewFlowService,
		ProvideAnalysisService,
		// The library and the analytics page resolve a scope's flow set
		// (owner / shared / org membership) through the same resolver.
		service.NewFlowScopeResolver,
		// Adapter: fx only provides the full storageif.StorageBackend, but
		// DashboardService's constructor takes the narrower DashboardStore and
		// the ScopeResolver interface; fx's reflection-based provider matching
		// needs this bridge.
		func(backend storageif.StorageBackend, analysis *service.AnalysisService, flowSvc *service.FlowService, scopes *service.FlowScopeResolver) *service.DashboardService {
			svc := service.NewDashboardService(backend, analysis, flowSvc, scopes)
			// Event hook (Phase 2b's bus replaces it): the dashboard cache is
			// user-keyed and must drop when the owner's flows change, but
			// DashboardService already depends on FlowService, so the hook is
			// registered here (a constructor parameter would cycle).
			flowSvc.SetCloudFlowMutationHook(svc.InvalidateDashboardCache)
			return svc
		},
		service.NewExportService,
		service.NewLibraryService,
		ProvidePolicyService,
		ProvideGovernanceAlertService,
		ProvideOrgRuleService,
		ProvideChannelService,
		ProvideUserDirectory,
		ProvideTriageService,
		ProvideCommentService,
		ProvideShareTokenService,
		// Adapter: the collaborator service takes the narrower store it uses (nil on the desktop).
		func(backend storageif.StorageBackend) *service.CollaboratorService {
			return service.NewCollaboratorService(backend)
		},
		// Adapter: fx only provides the full storageif.StorageBackend, but
		// AuthService's constructor takes the narrower authStore it actually
		// uses (same rationale as the DashboardService adapter above).
		func(backend storageif.StorageBackend, email *mail.Service) *service.AuthService {
			return service.NewAuthService(backend, email)
		},
		// Account and identity services, over the narrow store roles their
		// constructors take (same adapter rationale). Without a server store
		// (desktop) the roles are nil; the routes that use them declare
		// route.Needs(capability.ServerStore) or check for SSO first.
		func(backend storageif.StorageBackend, links service.IdentityLinkStore) *service.IdentityService {
			return service.NewIdentityService(backend, links)
		},
		service.NewSessionService,
		func(backend storageif.StorageBackend) *service.APITokenService {
			return service.NewAPITokenService(backend)
		},
		func(backend storageif.StorageBackend) *service.UserAdminService {
			return service.NewUserAdminService(backend)
		},
		service.NewHealthService,
		// Account erasure cascades the user's provider keys in SQL; finalize
		// drops the provider service's cached live listings for that scope so
		// they cannot outlive the account. The provider service also releases
		// the user's own Azure AI Foundry credential (read in prepare, before
		// the cascade) and disables every organisation credential the user lent
		// as an admin (org_provider_keys rows do not cascade with the account).
		func(backend storageif.StorageBackend, providers *service.ProviderService) *service.AccountLifecycleService {
			svc := service.NewAccountLifecycleService(backend, providers)
			svc.OnUserDeleted(providers.InvalidateScope)
			return svc
		},
		func(
			notifier service.EventNotifier,
			configDir string,
			flowSvc *service.FlowService,
			analysisSvc *service.AnalysisService,
			settings service.SettingsProvider,
			factory *ai.ProviderFactory,
			backend storageif.StorageBackend,
			knowledge *rag.KnowledgeService,
			mode config.DeploymentMode,
			redisClient *redis.Client,
			azure *service.AzureModelService,
			budget *service.AIBudget,
		) *service.ChatService {
			svc := service.NewChatService(notifier, configDir, flowSvc, analysisSvc, settings, factory, backend, mode, service.ChatOptions{
				Knowledge: knowledge,
				// The budget the knowledge base consults too (R-4).
				Budget: budget,
				// Chat turns and context previews on azure-foundry resolve their
				// model id on the Azure AI Foundry model registry and run on
				// exactly that entry's resource.
				Azure: azure,
				// nil when PAD_REDIS_URL is unset → single-replica resume (local maps).
				Resume: redisClient,
			})
			// Event hook (Phase 2b's bus replaces it): drop the scrubbed-context
			// cache when a flow changes in place, routed through FlowService so
			// LibraryService has no direct chat dep.
			flowSvc.OnInvalidateFlow(svc.InvalidateChatContext)
			return svc
		},
		func(
			auth *ai.GitHubAuth,
			copilot *ai.CopilotAuth,
			factory *ai.ProviderFactory,
			secrets service.KeyStore,
			orgKeys service.OrgKeyStore,
			sys *service.SystemService,
			azure *service.AzureModelService,
		) *service.ProviderService {
			// orgKeys: organisation-shared credentials (G7), nil outside cloud
			// mode, where the org routes report ORG_KEYSTORE_UNAVAILABLE. azure:
			// the Azure AI Foundry model registry — the single-document routes
			// act on its resource "default", the lifecycle and "configured"
			// checks on every resource.
			svc := service.NewProviderService(auth, copilot, factory, secrets, orgKeys, azure)
			// Event hook (Phase 2b's bus replaces it). POST /api/keys/save and
			// /api/keys/delete go through SystemService (removing an Azure AI
			// Foundry configuration too): drop that provider's cached live
			// listing and fence off one in flight — a replacement key may carry
			// other entitlements than the one it replaces.
			sys.OnApiKeyChanged(svc.InvalidateModels)
			return svc
		},
	),
	// R4: the deployment custom-rule file is its OWN graph node rather than
	// something the AnalysisService loads for itself: the service (fallback
	// layer) and the RuleProfileResolver (base layer) both need the same
	// compiled slice, and making the resolver read it off the service would
	// cycle (the service also needs the resolver).
	fx.Provide(ProvideDeploymentCustomRules),
	fx.Provide(service.NewRuleProfileResolver),
	// FlowService needs the rule resolver too: its batch-fix loop re-analyzes the
	// flow to find what to fix, and without the org profile that analysis ran
	// on deployment defaults — "fix all" could apply a fix for a rule the org
	// had disabled, and never applied an org custom rule's autoFix. A decorator,
	// not a constructor parameter, only because NewFlowService has ~55 test
	// call sites (no cycle; docs/architecture/di-wiring.md).
	fx.Decorate(func(resolver *service.RuleProfileResolver, svc *service.FlowService) *service.FlowService {
		svc.SetRuleProfileResolver(resolver)
		return svc
	}),
)

// ProvideDeploymentCustomRules compiles PAD_CUSTOM_RULES once at boot.
// Invalid entries are SKIPPED WITH A WARNING EACH, never silently: R0-5 was
// motivated by exactly that failure mode — a typo'd regex quietly removed a
// rule the operator believed was enforcing. A missing/unset path is a no-op.
func ProvideDeploymentCustomRules(cfg *config.Config) service.DeploymentCustomRules {
	path := cfg.Server.CustomRulesPath
	if path == "" {
		return nil
	}
	rules, warnings, err := analyzer.LoadCustomRules(path)
	if err != nil {
		// Don't fail boot: the deployment still has its 44 built-in rules.
		logger.Error("custom rules load failed; continuing with built-in rules only", "path", path, "error", err)
		return nil
	}
	for _, w := range warnings {
		logger.Warn("custom rule skipped", "file", path, "detail", w)
	}
	return rules
}
