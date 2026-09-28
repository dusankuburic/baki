package di

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/rag"
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

// R-4 wiring: the knowledge base consults the chat service's daily budget,
// so an upload on a spent budget is refused with its sentinel before any
// embedding is attempted.
func TestWireKnowledgeBudget(t *testing.T) {
	backend := testutil.NewFakeBackend()
	backend.DailyUsage = 10
	chat := service.NewChatService(nil, t.TempDir(), nil, nil, budgetSettings{budget: 5}, nil, backend, config.ModeCloud)
	ks := rag.NewKnowledgeService(testutil.NewFakeBackend(), nil, nil)
	wireKnowledgeBudget(ks, chat)
	err := ks.AddDocument(context.Background(), "u1", "org-1", "a.md", "content")
	if !errors.Is(err, service.ErrAIBudgetExceeded) {
		t.Errorf("upload on a spent budget: %v, want ErrAIBudgetExceeded", err)
	}
	wireKnowledgeBudget(nil, chat) // no knowledge service: no-op
}
