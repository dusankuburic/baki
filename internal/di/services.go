package di

import (
	"context"
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

func ProvideSettingsStore() (*storage.SettingsStore, error) {
	return storage.NewSettingsStore()
}

func ProvideConfigDir() (string, error) {
	return storage.ConfigDir()
}

func ProvideASTCache() (cache.Cache, error) {
	return cache.NewLRUCache(100) // Cache up to 100 flows
}

// ProvideHistoryStore persists per-flow analysis trend snapshots under the
// app config dir (HistoryStore treats an empty dir as a no-op).
func ProvideHistoryStore(configDir string) *analyzer.HistoryStore {
	return analyzer.NewHistoryStore(filepath.Join(configDir, "analysis-history"))
}

func ProvideAI(configDir string, backend storageif.StorageBackend, cfg *config.Config, keys service.KeyStore) (*ai.GitHubAuth, *ai.CopilotAuth, *ai.ProviderFactory, *ai.DemoLimiter) {
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
	demo := ai.NewDemoLimiter(configDir)
	return auth, copilotAuth, factory, demo
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
		// Adapter: expose *storage.SettingsStore as service.SettingsProvider
		// so services depend on the interface, not the concrete filesystem
		// implementation.
		func(s *storage.SettingsStore) service.SettingsProvider { return s },
		// WebhookNotifier is a thin facade over the shared notify.Dispatcher,
		// consolidating analysis-complete alerts onto the same delivery stack as
		// governance (scanner) alerts. The dispatcher is provided by
		// main.go's provideNotifier.
		service.NewWebhookNotifier,
		// service.KeyStore itself comes from main.go's provideKeyStore (a
		// plain fx provider, not a global) — no adapter needed here.
		func(backend storageif.StorageBackend, factory *ai.ProviderFactory, settings *storage.SettingsStore) *rag.KnowledgeService {
			// Pass the factory (not a pre-resolved provider) so the embedding
			// provider is resolved per request in the caller's scope. Resolving
			// once here with an empty scope fails in cloud mode (keys are
			// per-user) and never picks up keys added later.
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
			demo *ai.DemoLimiter,
			backend storageif.StorageBackend,
			knowledge *rag.KnowledgeService,
			mode config.DeploymentMode,
			redisClient *redis.Client,
		) *service.ChatService {
			svc := service.NewChatService(notifier, configDir, flowSvc, analysisSvc, settings, factory, demo, backend, mode)
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
	// Custom-rules loading: a DECORATOR (not a second provider of the same
	// type — fx hard-fails that graph with "already provided"). It wraps the
	// AnalysisService the constructor built, loading the operator's custom
	// rules when PAD_CUSTOM_RULES is configured.
	fx.Decorate(func(cfg *config.Config, svc *service.AnalysisService) *service.AnalysisService {
		if cfg.Server.CustomRulesPath != "" {
			svc.LoadCustomRules(cfg.Server.CustomRulesPath)
		}
		return svc
	}),
)
