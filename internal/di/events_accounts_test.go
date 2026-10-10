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
	"pad-analyzer/internal/testutil"
	"pad-core/analyzer"
)

// The graph subscribes the rule profiles to the bus its publishers announce on: an org rule saved through the
// service shows in the org's next resolve, not after the resolver's TTL.
func TestServiceModule_RuleProfilesFollowOrgRuleChanges(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeCloud
	cfg.Auth.Enabled = true
	backend := testutil.NewFakeBackend()
	var (
		rules    *service.OrgRuleService
		resolver *service.RuleProfileResolver
	)
	app := fx.New(
		fx.NopLogger,
		fx.Provide(
			func() context.Context { return context.Background() },
			func() *config.Config { return cfg },
			func(cfg *config.Config) config.DeploymentMode { return cfg.Mode },
			func() storageif.StorageBackend { return backend },
			func() service.EventNotifier { return service.NilNotifier{} },
			func() *collaboration.OrgService { return nil },
		),
		noDatabase(),
		fakeKeyStore(),
		ServiceModule,
		redisx.Module,
		fx.Populate(&rules, &resolver),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx := context.Background()
	has := func() bool {
		for _, r := range resolver.Resolve(ctx, "org-1").Rules {
			if r.ID() == "org-rule" {
				return true
			}
		}
		return false
	}
	if has() {
		t.Fatal("precondition: the rule does not exist yet")
	}
	rule := analyzer.CustomRuleConfig{ID: "org-rule", Name: "org-rule", Severity: "warning", Category: "Style", RawTypeMatch: "^SET$"}
	if _, err := rules.SaveRule(ctx, "org-1", service.OrgRuleSave{RowID: "r1", Config: &rule}); err != nil {
		t.Fatal(err)
	}
	if !has() {
		t.Error("the org's cached rule profile survived a rule saved through the service")
	}
}
