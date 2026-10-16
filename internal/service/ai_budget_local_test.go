package service

import (
	"context"
	"errors"
	"testing"
	"time"

	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil/pgtest"
	"pad-core/models"
)

// TestAIBudget_LocalUserCountsTheirOrgSpend: without accounts (local mode, JWT off) every usage row has the empty
// user, the local user's org spend included. The personal budget and the per-user sub-limit of an org chat read the
// local user's total, org rows included (ai_budget.go's contract), as on a deployment with accounts; the organisation
// spend that erasure anonymises cannot meet them, since erasure needs accounts.
func TestAIBudget_LocalUserCountsTheirOrgSpend(t *testing.T) {
	ctx := context.Background()
	pg, err := storagedb.New(ctx, storagedb.DefaultConfig(pgtest.NewDatabase(t, "")))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	now := time.Now().UTC()
	for _, m := range []*storageif.UsageMetric{
		{ID: "local-own", OrgID: "", Provider: "openai", Model: "m", EstimatedCost: 1.0, CreatedAt: now},
		{ID: "local-org", OrgID: "org-1", Provider: "openai", Model: "m", EstimatedCost: 2.0, CreatedAt: now},
		{ID: "local-yesterday", OrgID: "", Provider: "openai", Model: "m", EstimatedCost: 8.0, CreatedAt: now.Add(-48 * time.Hour)},
	} {
		if err := pg.SaveUsageMetric(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := pg.GetDailyUsage(ctx, "", ""); err != nil || got != 3.0 {
		t.Errorf("the local user's spend today = %v, %v; want 3 (their org spend included)", got, err)
	}

	// The per-user sub-limit of an org chat: $3 spent against $2.50.
	perUser := NewAIBudget(&staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 100, PerUserDailyBudget: 2.5}}}, pg)
	if err := perUser.CheckBudget(ctx, "", "org-1"); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Errorf("an org chat over the local user's sub-limit: %v, want ErrAIBudgetExceeded", err)
	}
	// The personal budget: $3 against $2.50.
	personal := NewAIBudget(&staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 2.5}}}, pg)
	if err := personal.CheckBudget(ctx, "", ""); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Errorf("a personal chat over the budget: %v, want ErrAIBudgetExceeded", err)
	}
}
