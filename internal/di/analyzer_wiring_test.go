package di

import (
	"context"
	"testing"

	"go.uber.org/fx"

	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-core/models"
)

// The graph builds one cached analyzer (ADR 0017) and hands it to the analysis service, which serves reports from
// it, and to the flow service, which drops them: on the desktop, opening a folder through the flow service resets
// the session analytics the analysis service reports.
func TestServiceModule_SharesOneAnalyzer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("AppData", home)
	t.Setenv("HOME", home)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	var (
		shared   *analyzer.CachedAnalyzer
		analysis *service.AnalysisService
		flows    *service.FlowService
	)
	app := fx.New(
		fx.NopLogger,
		fx.Provide(
			func() context.Context { return context.Background() },
			func() *config.Config { return cfg },
			func(cfg *config.Config) config.DeploymentMode { return cfg.Mode },
			func() storageif.StorageBackend { return nil },
			func() service.EventNotifier { return service.NilNotifier{} },
			func() *collaboration.OrgService { return nil },
		),
		noDatabase(),
		fakeKeyStore(),
		ServiceModule,
		redisx.Module,
		fx.Populate(&shared, &analysis, &flows),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("build: %v", err)
	}

	doc := &models.FlowDocument{ID: "d1", Name: "Flow", FilePath: home + "/flow.txt", Subflows: []models.Subflow{{ID: "sf", Name: "Main",
		Blocks: []models.Block{{ID: "b1", SubflowID: "sf", Name: "Set X", Type: models.BlockTypeAction, RawType: "SetVariable.Set", LineNumber: 1}}}}}
	report, err := analysis.AnalyzeFlow(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if got := shared.Reports(cache.Local()); len(got) != 1 || got[0] != report {
		t.Fatalf("the graph's analyzer holds %d reports, want the analysis service's one", len(got))
	}
	flows.ResetSessionAnalytics()
	if n := analysis.ComputeDashboard().TotalFlowsAnalyzed; n != 0 {
		t.Errorf("after the flow service's reset the session analytics count %d flows, want 0", n)
	}
}
