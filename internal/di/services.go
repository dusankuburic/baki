package di

import (
	"go.uber.org/fx"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/cache"
	"pad-analyzer/internal/config"
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

func ProvideAI(configDir string) (*ai.GitHubAuth, *ai.CopilotAuth, *ai.ProviderFactory, *ai.DemoLimiter) {
	copilotAuth := ai.NewCopilotAuth()
	factory := ai.NewProviderFactory(storage.GetApiKeyScoped, copilotAuth)
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
		ProvideAI,
		func(settings *storage.SettingsStore, notifier service.Notifier, backend storageif.StorageBackend) *service.SystemService {
			return service.NewSystemService(settings, notifier, backend)
		},
		service.NewFlowService,
		service.NewAnalysisService,
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
		) *service.ChatService {
			return service.NewChatService(notifier, configDir, flowSvc, analysisSvc, settings, factory, demo)
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
