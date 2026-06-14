package di

import (
	"context"
	"path/filepath"

	"go.uber.org/fx"

	"pad-analyzer/internal/ai"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
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

func ProvideAI(configDir string, backend storageif.StorageBackend, cfg *config.Config) (*ai.GitHubAuth, *ai.CopilotAuth, *ai.ProviderFactory, *ai.DemoLimiter) {
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
	factory := ai.NewProviderFactory(storage.GetApiKeyScoped, copilotAuth, recorder, &cfg.Runtime)
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
		func(backend storageif.StorageBackend, factory *ai.ProviderFactory, settings *storage.SettingsStore) *rag.KnowledgeService {
			// Pass the factory (not a pre-resolved provider) so the embedding
			// provider is resolved per request in the caller's scope. Resolving
			// once here with an empty scope fails in cloud mode (keys are
			// per-user) and never picks up keys added later.
			return rag.NewKnowledgeService(backend, factory, settings)
		},
		func(settings *storage.SettingsStore, notifier service.Notifier, backend storageif.StorageBackend) *service.SystemService {
			return service.NewSystemService(settings, notifier, backend)
		},
		service.NewAuthzService,
		service.NewFlowService,
		service.NewAnalysisService,
		service.NewDashboardService,
		service.NewExportService,
		service.NewLibraryService,
		func(
			notifier service.Notifier,
			configDir string,
			flowSvc *service.FlowService,
			analysisSvc *service.AnalysisService,
			settings *storage.SettingsStore,
			factory *ai.ProviderFactory,
			demo *ai.DemoLimiter,
			backend storageif.StorageBackend,
			knowledge *rag.KnowledgeService,
		) *service.ChatService {
			svc := service.NewChatService(notifier, configDir, flowSvc, analysisSvc, settings, factory, demo, backend)
			svc.SetKnowledgeService(knowledge)
			return svc
		},
		func(
			auth *ai.GitHubAuth,
			copilot *ai.CopilotAuth,
			factory *ai.ProviderFactory,
		) *service.ProviderService {
			return service.NewProviderService(auth, copilot, factory)
		},
	),
)
