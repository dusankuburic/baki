package di

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"pad-core/logger"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/lock"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
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

// ProvideASTCache builds the parsed-document cache (declared "flow.ast"), each
// document charged by the memory-audit cost model (blocks × 1 KiB + the source
// it carries).
func ProvideASTCache() *service.ASTCache {
	return service.NewASTCache(astCacheBudget)
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
// The knowledge base's pinned embedding provider must be a registered, embedding-capable spec (config checks
// its own copy of the ids; the registry is the authority). Every chain the factory builds consults the daily
// AI budget before each billed call and the provider policy before any key is read (ADR 0028); both depend on
// settings and the backend only, so there is no cycle through the services that take the factory.
func ProvideAI(configDir string, backend storageif.StorageBackend, cfg *config.Config, keys service.KeyStore, orgKeys service.OrgKeyStore, budget *service.AIBudget, policy *service.ProviderPolicy) (*ai.GitHubAuth, *ai.CopilotAuth, *ai.ProviderFactory, error) {
	if p := cfg.Storage.EmbeddingProvider; p != "" && !ai.SupportsEmbeddings(p) {
		return nil, nil, nil, fmt.Errorf("PAD_EMBEDDING_PROVIDER=%q is not an embedding-capable provider (registered: %s)", p, strings.Join(ai.EmbeddingFallbackOrder(), ", "))
	}
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
	// A nil *AIBudget or *ProviderPolicy (a di-only test graph) must not become a non-nil interface.
	var opts []ai.FactoryOption
	if budget != nil {
		opts = append(opts, ai.WithBudgetChecker(budget))
	}
	if policy != nil {
		opts = append(opts, ai.WithProviderPolicy(policy))
	}
	// The resilience policy, parsed once at load: the factory used to keep &cfg.Runtime and re-read it on every
	// chain build.
	factory := ai.NewProviderFactory(memberKeyLookup(keys), copilotAuth, recorder, &ai.ResilienceConfig{
		Threshold: cfg.Runtime.CircuitBreakerFailures,
		OpenFor:   cfg.Runtime.CircuitBreakerOpenDur.Std(),
		Attempts:  cfg.Runtime.RetryMaxAttempts,
		BaseDelay: cfg.Runtime.RetryBaseDelay.Std(),
	}, opts...)
	if orgKeys != nil {
		factory.SetOrgKeyLookup(orgKeyLookup(orgKeys))
	}
	// Embeddings come back at the knowledge base's vector width where the
	// model supports it, so they are indexable. The width is the storage
	// contract's, default included: passing the raw PAD_EMBEDDING_DIM (0 when
	// unset) asked for nothing, and text-embedding-3-large then returned
	// 3072-wide vectors against the 1536 index.
	factory.SetEmbeddingDimensions(cfg.Storage.EffectiveEmbeddingDim())
	// One egress policy, applied to every provider by its spec's EgressPolicy: the user-endpoint family gets the
	// dial-time guard and the allow-list; the local models provider (Foundry Local, Ollama, LM Studio) exists
	// only where loopback is allowed — the desktop/local single-user app, exactly the opt-in it needs.
	ai.ConfigureProviders(ai.ProviderOptions{
		Egress:             azureEndpointPolicy(capability.Of(cfg)),
		ExtraAllowedHosts:  cfg.Runtime.AzureFoundryAllowedHosts,
		AllowPrivateEgress: cfg.Runtime.AIAllowPrivateEgress,
	})
	auth := ai.NewGitHubAuth()
	return auth, copilotAuth, factory, nil
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

// azureEndpointPolicy is the egress policy of the user-supplied Azure AI Foundry endpoints: the multi-tenant
// server sends every request to one through the dial-time private-address guard (the operator can lift it for
// private-link resources); the desktop and the local development profiles talk to their owner's own network and
// need none. The AI endpoints keep this policy of their own (egress.ForDeployment is the tenant channels').
func azureEndpointPolicy(caps capability.Set) egress.Policy {
	if caps.Profile() == capability.Cloud {
		return egress.BlockInternal
	}
	return egress.AllowLoopback
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

// ProvideAnalyzer is the process's one cached analyzer (ADR 0017): the analysis service serves its reports and
// inventories, and the flow service drops them when a flow is reimported or deleted, or the desktop opens a folder.
func ProvideAnalyzer() *analyzer.CachedAnalyzer {
	return service.NewDefaultAnalyzer()
}

// ProvideAnalysisService builds the AnalysisService on the deployment's custom rules and the per-org rule
// resolver (the resolver takes the same compiled slice, ProvideDeploymentCustomRules).
func ProvideAnalysisService(notifier service.EventNotifier, settings service.SettingsProvider, history *analyzer.HistoryStore, cfg *config.Config, rules service.DeploymentCustomRules, resolver *service.RuleProfileResolver, bus *eventbus.Bus, a *analyzer.CachedAnalyzer) (*service.AnalysisService, error) {
	return service.NewAnalysisService(notifier, settings, history, service.AnalysisOptions{
		CustomRules: rules,
		Profiles:    resolver,
		// With auth on, an analysis that carries no user (the scanner) must not
		// broadcast progress to every connected client.
		AuthEnabled: cfg.Auth.Enabled,
		Bus:         bus,
		Analyzer:    a,
		// Folder batch analysis, which runs the library rules, is refused with accounts.
		LibraryRules: !capability.Of(cfg).Has(capability.Accounts),
	})
}

// ProvideAIBudget builds the daily AI budget gate chat turns, the knowledge base's billed embedding runs and
// every provider chain's pre-call hook (ProvideAI) share (R-4, ADR 0028): one in-flight ledger, so an
// embedding run counts what running chat streams have committed. backend is nil on the desktop (no usage
// store: spend reads $0, so only the in-flight ledger can trip there).
func ProvideAIBudget(settings service.SettingsProvider, backend storageif.StorageBackend) *service.AIBudget {
	return provideAIBudget(settings, backend, nil, nil, nil)
}

// provideAIBudget is ProvideAIBudget with the in-flight ledger in Redis when the deployment has more than one
// replica configured (PAD_MAX_REPLICAS > 1) and a Redis client: only then does a stream run somewhere a budget
// check on another replica cannot see. A single replica keeps the in-process ledger even with PAD_REDIS_URL set
// (rate limits, rooms and resume use it), because the Redis ledger fails closed: with it, a Redis outage would
// stop AI on a deployment that has nothing to share. With a daily budget set, an unreachable Redis then refuses new
// turns and stops running ones at their next budget re-check (ADR 0028 addendum, docs/architecture/ai-providers.md).
func provideAIBudget(settings service.SettingsProvider, backend storageif.StorageBackend, redisClient *redis.Client, health *redisx.Health, cfg *config.Config) *service.AIBudget {
	return service.NewAIBudget(settings, backend, service.WithRedisLedger(sharedLedgerClient(redisClient, cfg), health))
}

// sharedLedgerClient is the client the budget ledger lives on: the shared one with several replicas, else nil (the
// in-process ledger).
func sharedLedgerClient(redisClient *redis.Client, cfg *config.Config) *redis.Client {
	if cfg == nil || cfg.Storage.MaxReplicas <= 1 {
		return nil
	}
	return redisClient
}

// ProvideProviderPolicy builds the provider policy every factory builder consults (ADR 0028): the
// deployment's Enabled flags from the settings, the organisation's allowed-provider list from its settings
// document. backend is nil on the desktop: no organisations, no org list.
func ProvideProviderPolicy(settings service.SettingsProvider, backend storageif.StorageBackend) *service.ProviderPolicy {
	var orgs service.OrgSettingsReader
	if backend != nil {
		orgs = backend
	}
	return service.NewProviderPolicy(settings, orgs)
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
// leaser is the jobs' lease (nil on the desktop): the background re-embed job runs under it, one replica at a time, and
// so does every org upload and re-index (a lease per org over the check -> embed -> write window, at most
// PAD_KNOWLEDGE_LEASE_CAP held per replica).
func ProvideKnowledgeService(backend storageif.StorageBackend, factory *ai.ProviderFactory, settings service.SettingsProvider, cfg *config.Config, azure *service.AzureModelService, budget *service.AIBudget, bus *eventbus.Bus, leaser lock.Leaser) *rag.KnowledgeService {
	opts := []rag.Option{rag.WithEmbeddingProvider(cfg.Storage.EmbeddingProvider), rag.WithEventBus(bus)}
	if leaser != nil {
		opts = append(opts, rag.WithJobLeaser(leaser), rag.WithLeaseCap(cfg.Storage.EffectiveKnowledgeLeaseCap()))
	}
	if azure != nil {
		opts = append(opts, rag.WithAzureEmbedder(azure))
	}
	if budget != nil {
		opts = append(opts, rag.WithBudgetChecker(budget))
	}
	return rag.NewKnowledgeService(backend, factory, settings, opts...)
}

// knowledgeDeps are ProvideKnowledgeService's inputs; the leaser is internal/app's (nil without a database, absent
// from the di-only test graphs), so it is optional.
type knowledgeDeps struct {
	fx.In
	Backend  storageif.StorageBackend
	Factory  *ai.ProviderFactory
	Settings service.SettingsProvider
	Cfg      *config.Config
	Azure    *service.AzureModelService
	Budget   *service.AIBudget
	Bus      *eventbus.Bus
	Leaser   lock.Leaser `optional:"true"`
}

func provideKnowledgeService(d knowledgeDeps) *rag.KnowledgeService {
	return ProvideKnowledgeService(d.Backend, d.Factory, d.Settings, d.Cfg, d.Azure, d.Budget, d.Bus, d.Leaser)
}

// azureDatabaseRegistry is the database's registry when the registry lives
// there: with authentication on, since only then are owner ids real users and
// organisations and the foreign keys (and their cascades) hold. nil otherwise.
func azureDatabaseRegistry(cfg *config.Config, db storagedb.AzureRegistryProvider) storagedb.AzureRegistryProvider {
	if !cfg.Auth.Enabled {
		return nil
	}
	return db
}

// ProvideAzureRegistry selects where the Azure AI Foundry model registry
// lives: Postgres (azureDatabaseRegistry), else the per-user config
// directory's azure-models.json (the desktop, and a database with auth off),
// whose one personal registry is owner "". Never settings.json. db is the
// database's registry provider, nil without a database. orgKeys only decides
// whether organisations can hold registries (logged). Without a config
// directory the registry is kept in memory, with a warning.
func ProvideAzureRegistry(cfg *config.Config, db storagedb.AzureRegistryProvider, orgKeys service.OrgKeyStore, configDir string) (storageif.AzureRegistryStore, error) {
	if p := azureDatabaseRegistry(cfg, db); p != nil {
		logger.Info("azure registry: database store", "organisationRegistries", orgKeys != nil)
		return p.AzureRegistry(), nil
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
// each personal resource's public configuration through it. keyFamilies and
// orgKeys are the database keystore's (nil with the OS keyring).
func ProvideAzureModelService(registry storageif.AzureRegistryStore, secrets service.KeyStore, keyFamilies storageif.SecretFamilyLister, orgKeys service.OrgKeyStore, factory *ai.ProviderFactory, settings service.SettingsProvider, backend storageif.StorageBackend, db storagedb.AzureRegistryProvider, cfg *config.Config) *service.AzureModelService {
	src := azureDatabaseRegistry(cfg, db)
	perUser := src != nil
	deps := service.AzureModelServiceDeps{
		Registry:      registry,
		Secrets:       secrets,
		KeyFamilies:   keyFamilies,
		OrgKeys:       orgKeys,
		Factory:       factory,
		Settings:      settings,
		PerUserOwners: perUser,
		// The shared corpus is the organisations' knowledge base: a server store with accounts.
		KnowledgeEmbeddingEnabled: capability.Of(cfg).Has(capability.ServerStore, capability.Accounts) && cfg.Storage.EmbeddingProvider == ai.AzureFoundryProviderID,
	}
	if backend != nil {
		deps.UserSettings, deps.Orgs, deps.Corpus, deps.Users = backend, backend, backend, backend
	}
	svc := service.NewAzureModelService(deps)
	if perUser {
		src.SetAzureExportConfigSource(svc.ExportConnectionConfigs)
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

func ProvideOrgRuleService(backend storageif.StorageBackend, bus *eventbus.Bus) *service.OrgRuleService {
	return service.NewOrgRuleService(backend, backend, bus)
}

// ProvideChannelService is the organisations' notification channels under the deployment's egress policy and host
// allow-list (PAD_NOTIFY_ALLOWED_HOSTS); its delivery runner serves the scanner and the channel test, and its email
// channels send through the mail service.
func ProvideChannelService(backend storageif.StorageBackend, cfg *config.Config, email *mail.Service) *service.ChannelService {
	return service.NewChannelService(backend, service.ChannelOptions{
		Policy:              egress.ForDeployment(capability.Of(cfg)),
		AllowedHosts:        cfg.Governance.NotifyAllowedHosts,
		AllowedHostsVisible: cfg.Governance.NotifyAllowedHostsVisible,
		Mailer:              email,
		Members:             backend,
	})
}

func ProvideUserDirectory(backend storageif.StorageBackend) *service.UserDirectory {
	return service.NewUserDirectory(backend)
}

// ProvideTriageService, ProvideCommentService and ProvideShareTokenService
// narrow the full backend to the role interfaces each service takes (fx only
// provides storageif.StorageBackend). On the desktop the backend is nil and so
// are the roles; the routes that reach these services need the server-side
// store.
func ProvideTriageService(backend storageif.StorageBackend, bus *eventbus.Bus) *service.TriageService {
	return service.NewTriageService(backend, backend, backend, bus)
}

func ProvideCommentService(backend storageif.StorageBackend, triage *service.TriageService, bus *eventbus.Bus) *service.CommentService {
	return service.NewCommentService(backend, triage, bus)
}

func ProvideShareTokenService(backend storageif.StorageBackend, bus *eventbus.Bus) *service.ShareTokenService {
	return service.NewShareTokenService(backend, backend, bus)
}

// ProvideCIService builds the CI webhook's authentication and the organisations' CI credentials (ADR 0019): with
// accounts, organisations mint their own, API tokens with the ci scope reach the flows their user can read (authz),
// and the deployment-wide PAD_CI_WEBHOOK_SECRET is deprecated (warned here, once per boot); without, that secret is
// the webhook's only key.
func ProvideCIService(cfg *config.Config, backend storageif.StorageBackend, docs service.DocumentProvider, authz *service.AuthzService) *service.CIService {
	caps := capability.Of(cfg)
	gov := cfg.Governance
	if caps.Has(capability.ServerStore) && gov.InboundWebhookSecret.IsSet() && caps.Has(capability.Accounts) {
		logger.Warn("PAD_CI_WEBHOOK_SECRET is deprecated where organisations mint their own CI credentials: it reaches every organisation's flows. Move runners to organisation CI credentials (Settings > Organisation > CI credentials); calls signed with the secret are answered with a Deprecation header")
	}
	if gov.InboundWebhookSecret.IsSet() && gov.InboundWebhookAllowNoTimestamp {
		logger.Warn("PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP=1 is deprecated: the CI webhook accepts replayable signatures without X-Baki-Timestamp under PAD_CI_WEBHOOK_SECRET")
	}
	return service.NewCIService(backend, docs, authz, service.CIOptions{
		GlobalSecret:     gov.InboundWebhookSecret,
		AllowNoTimestamp: gov.InboundWebhookAllowNoTimestamp,
		OrgCredentials:   caps.Has(capability.Accounts),
	})
}

// ServiceModule bundles all internal services.
// Event bus limits: concurrent async deliveries (email, notifications, realtime) and the time each may take.
const (
	eventBusAsyncLimit   = 64
	eventBusAsyncTimeout = 30 * time.Second
)

// ProvideEventBus is the process's domain event bus (docs/adr/0009-domain-events.md). Its async deliveries run on a
// bounded runner that shutdown drains with the other background runners.
func ProvideEventBus() *eventbus.Bus {
	return eventbus.New(lifecycle.NewBounded("events", eventBusAsyncLimit), eventBusAsyncTimeout)
}

var ServiceModule = fx.Options(
	fx.Provide(
		ProvideEventBus,
		ProvideDocumentProvider,
		ProvideSettingsStore,
		ProvideConfigDir,
		ProvideASTCache,
		ProvideAnalyzer,
		ProvideHistoryStore,
		ProvideAI,
		ProvideSettingsProvider,
		// WebhookNotifier is a thin facade over the shared notify.Dispatcher,
		// consolidating analysis-complete alerts onto the same delivery stack as
		// governance (scanner) alerts; provided by internal/app's provideNotifier.
		service.NewWebhookNotifier,
		// service.KeyStore, service.OrgKeyStore and the key-family lister come
		// from internal/app's provideKeyStore (plain fx values, not globals).
		ProvideAzureRegistry,
		ProvideAzureModelService,
		provideAIBudget,
		ProvideProviderPolicy,
		provideKnowledgeService,
		// /api/keys/has and /api/keys/delete for azure-foundry act on the
		// model registry (any resource; resource "default").
		service.NewSystemService,
		service.NewAuthzService,
		ProvideFlowService,
		ProvideAnalysisService,
		// The library and the analytics page resolve a scope's flow set
		// (owner / shared / org membership) through the same resolver.
		service.NewFlowScopeResolver,
		// Adapter: fx only provides the full storageif.StorageBackend, but
		// DashboardService's constructor takes the narrower DashboardStore and
		// the ScopeResolver interface; fx's reflection-based provider matching
		// needs this bridge.
		func(backend storageif.StorageBackend, analysis *service.AnalysisService, flowSvc *service.FlowService, scopes *service.FlowScopeResolver, bus *eventbus.Bus) *service.DashboardService {
			return service.NewDashboardService(backend, analysis, flowSvc, scopes, service.WithDashboardEvents(bus))
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
		ProvideCIService,
		// Adapter: the collaborator service takes the narrower store it uses (nil on the desktop).
		func(backend storageif.StorageBackend, bus *eventbus.Bus) *service.CollaboratorService {
			return service.NewCollaboratorService(backend, bus)
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
		func(backend storageif.StorageBackend, links storageif.IdentityLinkStore) *service.IdentityService {
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
		// announces it (AccountErased), which drops the provider service's
		// cached live listings for that scope. The provider service also releases
		// the user's own Azure AI Foundry credential (read in prepare, before
		// the cascade) and disables every organisation credential the user lent
		// as an admin (org_provider_keys rows do not cascade with the account).
		func(backend storageif.StorageBackend, providers *service.ProviderService, bus *eventbus.Bus) *service.AccountLifecycleService {
			return service.NewAccountLifecycleService(backend, providers, bus)
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
			cfg *config.Config,
			lc fx.Lifecycle,
		) *service.ChatService {
			chat := service.NewChatService(notifier, configDir, flowSvc, analysisSvc, settings, factory, backend, mode, service.ChatOptions{
				Knowledge: knowledge,
				// The budget the knowledge base consults too (R-4).
				Budget: budget,
				// Chat turns and context previews on azure-foundry resolve their
				// model id on the Azure AI Foundry model registry and run on
				// exactly that entry's resource.
				Azure: azure,
				// nil when PAD_REDIS_URL is unset → single-replica resume (local maps).
				Resume: redisClient,
				// The replica's name: stamped, with a boot suffix, in the resume snapshots and the address of its stream-command
				// channel.
				Instance: cfg.Runtime.InstanceID,
				// The commands are signed with a key derived from the deployment's shared secret.
				BusKey: commandBusKey(cfg, redisClient != nil),
			})
			// The replica listens for stream commands from start; Shutdown (the first stop hook) ends the subscriptions after
			// the streams (ADR 0033). A no-op without Redis.
			lc.Append(fx.Hook{OnStart: func(context.Context) error { chat.StartCommands(); return nil }})
			return chat
		},
		func(
			auth *ai.GitHubAuth,
			copilot *ai.CopilotAuth,
			factory *ai.ProviderFactory,
			secrets service.KeyStore,
			orgKeys service.OrgKeyStore,
			azure *service.AzureModelService,
			cfg *config.Config,
			bus *eventbus.Bus,
		) *service.ProviderService {
			// orgKeys: organisation-shared credentials (G7), nil outside cloud
			// mode, where the org routes report ORG_KEYSTORE_UNAVAILABLE. azure:
			// the Azure AI Foundry model registry — the single-document routes
			// act on its resource "default", the lifecycle and "configured"
			// checks on every resource. The registry is the azure-foundry
			// spec's Listing hook (ADR 0025), installed here because it is
			// built after the specs register.
			if hook := service.AzureListing(azure, orgKeys); hook != nil {
				ai.InstallListing(ai.AzureFoundryProviderID, hook)
			}
			return service.NewProviderService(auth, copilot, factory, secrets, orgKeys, azure, service.WithProfile(capability.Of(cfg).Profile()), service.WithProviderEvents(bus))
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
	fx.Invoke(SubscribeFlowCaches),
	fx.Invoke(SubscribeSettingsAndCredentialCaches),
)

// ProvideDeploymentCustomRules compiles PAD_CUSTOM_RULES once at boot, tolerantly (ADR 0015): an unknown field, a
// missing id or a built-in rule's id is a warning and the rule loads; an entry with an error (an invalid value, a
// kind or schemaVersion this build cannot run) is SKIPPED WITH A WARNING EACH, never silently: R0-5 was
// motivated by exactly that failure mode — a typo'd regex quietly removed a rule the operator believed was
// enforcing. A named file that does not exist is a WARN, not a silent no-op; an unset path is.
func ProvideDeploymentCustomRules(cfg *config.Config) service.DeploymentCustomRules {
	path := cfg.Server.CustomRulesPath
	if path == "" {
		return nil
	}
	rules, problems, err := analyzer.LoadCustomRuleFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		logger.Warn("custom rules file not found; continuing with built-in rules only", "path", path)
		return nil
	}
	if err != nil {
		// Don't fail boot: the deployment still has its built-in rules.
		logger.Error("custom rules load failed; continuing with built-in rules only", "path", path, "error", err)
		return nil
	}
	for _, p := range problems {
		msg := "custom rule skipped"
		if p.Warning {
			msg = "custom rule warning"
		}
		logger.Warn(msg, "file", path, "index", p.Index, "rule", p.RuleID, "field", p.Field, "reason", p.Reason, "detail", p.Error())
	}
	return rules
}

// commandBusKey is the HMAC key of the stream commands between replicas, derived from the deployment's shared secret
// (PAD_ENCRYPTION_KEY, else PAD_AUTH_SECRET). With none (auth off, or only a per-process secret) the commands go
// unsigned and the boot says so: whoever can publish on the Redis channels can then stop streams and decide
// approvals, which the private network and the Redis password are all that stands in front of.
func commandBusKey(cfg *config.Config, hasRedis bool) []byte {
	if !hasRedis {
		return nil
	}
	shared := cfg.Auth.SharedSecret()
	if !shared.IsSet() {
		logger.Warn("redis: no PAD_ENCRYPTION_KEY or PAD_AUTH_SECRET shared by the replicas: stream commands between replicas are not signed; " +
			"keep Redis on a private network with a password and TLS")
		return nil
	}
	return redisx.DeriveBusKey(shared.Reveal())
}
