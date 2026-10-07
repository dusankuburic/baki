package di

import (
	"context"
	"testing"

	"go.uber.org/fx"

	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// The services build — constructors run, not only the graph's types (TestAppGraph) — in each deployment
// mode, with the registry and the rule wiring reaching the services that take them as constructor
// parameters. The desktop has no backend; local+db and cloud run on one.
func TestServiceModule_BuildsInEveryMode(t *testing.T) {
	// The desktop modes resolve the per-user config directory: keep it out of the real one.
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("AppData", home)
	t.Setenv("HOME", home)

	modes := []struct {
		name    string
		mode    config.DeploymentMode
		backend func() storageif.StorageBackend
		auth    bool
	}{
		{"desktop", config.ModeLocal, func() storageif.StorageBackend { return nil }, false},
		{"local+db", config.ModeLocal, func() storageif.StorageBackend { return testutil.NewFakeBackend() }, false},
		{"cloud", config.ModeCloud, func() storageif.StorageBackend { return testutil.NewFakeBackend() }, true},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Mode = m.mode
			cfg.Auth.Enabled = m.auth
			var (
				analysis  *service.AnalysisService
				azure     *service.AzureModelService
				budget    *service.AIBudget
				chat      *service.ChatService
				dashboard *service.DashboardService
				knowledge *rag.KnowledgeService
				library   *service.LibraryService
				providers *service.ProviderService
				system    *service.SystemService
			)
			app := fx.New(
				fx.NopLogger,
				fx.Provide(
					func() context.Context { return context.Background() },
					func() *config.Config { return cfg },
					func(cfg *config.Config) config.DeploymentMode { return cfg.Mode },
					m.backend,
					func() service.EventNotifier { return service.NilNotifier{} },
					func() *collaboration.OrgService { return nil },
					func() service.KeyStore { return testutil.NewFakeSecretStore() },
				),
				ServiceModule,
				redisx.Module,
				fx.Populate(&analysis, &azure, &budget, &chat, &dashboard, &knowledge, &library, &providers, &system),
			)
			if err := app.Err(); err != nil {
				t.Fatalf("build: %v", err)
			}
			if providers.AzureModels() != azure {
				t.Error("the provider service does not hold the graph's Azure model registry")
			}
			if analysis.AuthEnabled() != m.auth {
				t.Errorf("AnalysisService.AuthEnabled() = %v, want %v", analysis.AuthEnabled(), m.auth)
			}
			if budget == nil || knowledge == nil || chat == nil || dashboard == nil || library == nil || system == nil {
				t.Error("a service was not built")
			}
		})
	}
}
