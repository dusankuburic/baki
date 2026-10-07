package di

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// budgetSettings is a settings provider with a fixed daily AI budget.
type budgetSettings struct{ budget float64 }

func (b budgetSettings) Get() *models.AppSettings {
	return &models.AppSettings{AI: models.AISettings{DailyBudget: b.budget}}
}
func (budgetSettings) Update(context.Context, models.AppSettings) error { return nil }
func (budgetSettings) AddRecentFile(string, int64) error                { return nil }
func (budgetSettings) RemoveRecentFile(string) error                    { return nil }
func (budgetSettings) ClearRecentFiles() error                          { return nil }

// R-4 wiring: the knowledge base consults the daily AI budget chat turns use
// (ProvideAIBudget), so an upload on a spent budget is refused with its
// sentinel before any embedding is attempted. The embedder is a priced
// (non-Azure) provider: the knowledge base resolves it before the budget
// check, and Azure AI Foundry is never budget-gated.
func TestProvideKnowledgeService_ConsultsTheAIBudget(t *testing.T) {
	backend := testutil.NewFakeBackend()
	backend.DailyUsage = 10
	settings := budgetSettings{budget: 5}
	factory := ai.NewProviderFactory(func(_, provider string) (string, error) {
		if provider == "openai" {
			return "sk-test", nil
		}
		return "", nil
	}, nil, nil, nil)
	cfg := config.Default()
	budget := ProvideAIBudget(settings, backend)

	err := ProvideKnowledgeService(testutil.NewFakeBackend(), factory, settings, cfg, nil, budget).AddDocument(context.Background(), "u1", "org-1", "a.md", "content")
	if !errors.Is(err, service.ErrAIBudgetExceeded) {
		t.Errorf("upload on a spent budget: %v, want ErrAIBudgetExceeded", err)
	}
	backend.UsageErr = errors.New("db down")
	if err := budget.CheckBudget(context.Background(), "u1", "org-1"); !errors.Is(err, service.ErrAIBudgetUnavailable) {
		t.Errorf("unreadable spend: %v, want ErrAIBudgetUnavailable (fail closed)", err)
	}
	// The desktop has no usage store: spend reads $0, within any budget.
	if err := ProvideAIBudget(settings, nil).CheckBudget(context.Background(), "local", ""); err != nil {
		t.Errorf("no usage store: %v", err)
	}
}
