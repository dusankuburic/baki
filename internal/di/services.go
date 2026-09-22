package di

import (
	"context"
	"pad-core/logger"
	"path/filepath"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/cache"
)

func ProvideDocumentProvider(mode config.DeploymentMode, storageBackend storageif.StorageBackend) service.DocumentProvider {
	if mode == config.ModeLocal {
		return service.NewLocalDocumentProvider()
	}
	return service.NewCloudDocumentProvider(storageBackend)
}

// ProvideSettingsStore builds the desktop app-settings store, backed by a file
// under the user's config directory. Cloud mode gets a memory-only instance
// because nothing there should be reading it — see ProvideSettingsProvider —
// and because a file-backed store would force the container to have a writable
// home directory.
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
// split deployment settings in two: POST /api/system/settings wrote to the
// database while POST /api/analysis/rule/enabled wrote to the local file, and
// every reader on the analysis path — rule profiles, the AI daily budget, the
// knowledge-base embedding provider — read the local file. In a container that
// file is recreated from defaults on every start, so an administrator's saved
// configuration had no effect on analysis at all.
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

// ProvideConfigDir resolves the per-user application directory. It is a
// desktop concept: in cloud mode the container's filesystem is ephemeral and
// (under the compose production overlay) read-only, so this returns an empty
// path and the stores that take it degrade to no-ops. Returning a path here
// also made boot depend on os.UserConfigDir() succeeding, i.e. on $HOME being
// set inside a distroless image.
func ProvideConfigDir(mode config.DeploymentMode) (string, error) {
	if mode == config.ModeCloud {
		return "", nil
	}
	return storage.ConfigDir()
}

func ProvideASTCache() (cache.Cache, error) {
	return cache.NewLRUCache(100) // Cache up to 100 flows
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

func ProvideAI(configDir string, backend storageif.StorageBackend, cfg *config.Config, keys service.KeyStore) (*ai.GitHubAuth, *ai.CopilotAuth, *ai.ProviderFactory) {
	copilotAuth := ai.NewCopilotAuth()
	// The storage backend is nil in local/desktop mode (no usage store). Leave the
	// recorder nil there — the audited provider guards a nil recorder and skips
	// recording — rather than handing it a closure that would dereference the nil
	// backend and panic in the goroutine record() spawns after every completion.
	var recorder ai.UsageRecorder
	if backend != nil {
		recorder = func(ctx context.Context, metric *storageif.UsageMetric) error {
			return backend.SaveUsageMetric(ctx, metric)
		}
	}
	factory := ai.NewProviderFactory(keys.Get, copilotAuth, recorder, &cfg.Runtime)
	auth := ai.NewGitHubAuth()
	return auth, copilotAuth, factory
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
		// governance (scanner) alerts. The dispatcher is provided by
		// main.go's provideNotifier.
		service.NewWebhookNotifier,
		// service.KeyStore itself comes from main.go's provideKeyStore (a
		// plain fx provider, not a global) — no adapter needed here.
		func(backend storageif.StorageBackend, factory *ai.ProviderFactory, settings service.SettingsProvider) *rag.KnowledgeService {
			// Pass the factory (not a pre-resolved provider) so the embedding
			// provider is resolved per request in the caller's scope. Resolving
			// once here with an empty scope fails in cloud mode (keys are
			// per-user) and never picks up keys added later.
			//
			// settings is the service.SettingsProvider, NOT the concrete
			// *storage.SettingsStore: in cloud mode the configured embedding
			// provider and model live in the database, and taking the concrete
			// store here read them from a local file that always held the
			// defaults.
			return rag.NewKnowledgeService(backend, factory, settings)
		},
		func(settings service.SettingsProvider, secrets service.KeyStore, notifier service.EventNotifier, backend storageif.StorageBackend, mode config.DeploymentMode) *service.SystemService {
			return service.NewSystemService(settings, secrets, notifier, backend, mode)
		},
		service.NewAuthzService,
		service.NewFlowService,
		service.NewAnalysisService,
		// Adapter: fx only provides the full storageif.StorageBackend, but
		// DashboardService's constructor takes the narrower DashboardStore it
		// actually uses — Go allows the wider interface value through at this
		// call site even though fx's reflection-based provider matching needs
		// the concrete registration to bridge the two static types.
		func(backend storageif.StorageBackend, analysis *service.AnalysisService, flowSvc *service.FlowService) *service.DashboardService {
			return service.NewDashboardService(backend, analysis, flowSvc)
		},
		service.NewExportService,
		service.NewLibraryService,
		// Adapter: fx only provides the full storageif.StorageBackend, but
		// AuthService's constructor takes the narrower authStore it actually
		// uses (same rationale as the DashboardService adapter above).
		func(backend storageif.StorageBackend, email *mail.Service) *service.AuthService {
			return service.NewAuthService(backend, email)
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
		) *service.ChatService {
			svc := service.NewChatService(notifier, configDir, flowSvc, analysisSvc, settings, factory, backend, mode)
			svc.SetKnowledgeService(knowledge)
			// nil when PAD_REDIS_URL is unset → single-replica resume (local maps).
			svc.SetResumeBackplane(redisClient)
			// Drop the scrubbed-context cache when a flow changes in place,
			// routed through FlowService so LibraryService has no direct chat dep.
			flowSvc.OnInvalidateFlow(svc.InvalidateChatContext)
			return svc
		},
		func(
			auth *ai.GitHubAuth,
			copilot *ai.CopilotAuth,
			factory *ai.ProviderFactory,
			secrets service.KeyStore,
		) *service.ProviderService {
			return service.NewProviderService(auth, copilot, factory, secrets)
		},
	),
	// R4: the deployment custom-rule file is its OWN graph node rather than
	// something the AnalysisService loads for itself. Two consumers need the
	// same compiled slice — the service (as its fallback layer) and the
	// RuleProfileResolver (as its base layer) — and making the resolver read it
	// off the service would be a cycle, since the service also needs the
	// resolver.
	fx.Provide(ProvideDeploymentCustomRules),
	fx.Provide(service.NewRuleProfileResolver),
	// A DECORATOR, not a second provider of *AnalysisService — fx hard-fails
	// that graph with "already provided". It wires both dependencies into the
	// service the constructor built.
	fx.Decorate(func(rules service.DeploymentCustomRules, resolver *service.RuleProfileResolver, svc *service.AnalysisService) *service.AnalysisService {
		svc.SetCustomRules(rules)
		svc.SetRuleProfileResolver(resolver)
		return svc
	}),
	// FlowService needs the same resolver: its batch-fix loop re-analyzes the
	// flow to find what to fix, and without the org profile that analysis ran
	// on deployment defaults — so "fix all" could apply a fix for a rule the
	// org had disabled, and never applied an org custom rule's autoFix. A
	// second decorator, because one fx.Decorate returns exactly one type.
	fx.Decorate(func(resolver *service.RuleProfileResolver, svc *service.FlowService) *service.FlowService {
		svc.SetRuleProfileResolver(resolver)
		return svc
	}),
)

// ProvideDeploymentCustomRules compiles PAD_CUSTOM_RULES once at boot.
//
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
