package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-config-1: in cloud mode the deployment settings document (the enforced AI budgets, the embedding choice) had
// no admin writer, and the ai.* values a user saves were never read.

// TestSetRuleEnabled_KeepsAnotherReplicasAIWrite: a rule write wrote back the WHOLE deployment document from its
// replica's 30 s cache, so a budget an admin had just set on another replica was silently reverted.
func TestSetRuleEnabled_KeepsAnotherReplicasAIWrite(t *testing.T) {
	ctx := context.Background()
	b := &countingBackend{}
	onA := NewBackendSettingsProvider(ctx, b)
	onB := NewBackendSettingsProvider(ctx, b) // primed before A's write; no invalidation reaches it
	written := onA.Get()
	written.AI.DailyBudget = 200
	if err := onA.Update(ctx, *written); err != nil {
		t.Fatal(err)
	}

	analysis, err := NewAnalysisService(&testutil.CountingNotifier{}, onB, nil, AnalysisOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.SetRuleEnabled(ctx, "hardcoded-url", false); err != nil {
		t.Fatalf("SetRuleEnabled: %v", err)
	}
	if err := analysis.UpdateRuleConfig(ctx, "deep-nesting", models.RuleConfig{Enabled: true, Severity: "low"}); err != nil {
		t.Fatalf("UpdateRuleConfig: %v", err)
	}

	stored, err := b.FakeBackend.LoadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AI.DailyBudget != 200 {
		t.Errorf("DailyBudget = %v after rule writes on another replica, want the admin's 200", stored.AI.DailyBudget)
	}
	if rc := stored.Analysis.Rules["hardcoded-url"]; rc.Enabled {
		t.Error("the rule write itself was lost")
	}
	if rc := stored.Analysis.Rules["deep-nesting"]; rc.Severity != "low" {
		t.Errorf("the rule config write was lost: %+v", rc)
	}
}

// TestUpdateUserSettings_IgnoresBudgetFields: budgets are enforced from the deployment document only, so a member's
// write leaves the budgets their own document carries as they are, and the enforced value does not move.
func TestUpdateUserSettings_IgnoresBudgetFields(t *testing.T) {
	ctx := context.Background()
	b := &countingBackend{FakeBackend: testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1"}}}}
	storedWithBudget(t, b, 200)
	sys := NewSystemService(NewBackendSettingsProvider(ctx, b), nil, &testutil.CountingNotifier{}, b, config.ModeCloud, nil, nil)

	before, err := b.LoadUserSettings(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	in := models.DefaultSettings()
	in.AI.DailyBudget = before.AI.DailyBudget + 1
	in.AI.PerUserDailyBudget = 999
	in.AI.ShowCostEstimates = !before.AI.ShowCostEstimates
	if err := sys.UpdateUserSettings(ctx, "u1", *in, nil); err != nil {
		t.Fatalf("UpdateUserSettings: %v", err)
	}
	mine, err := b.LoadUserSettings(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if mine.AI.DailyBudget != before.AI.DailyBudget || mine.AI.PerUserDailyBudget != before.AI.PerUserDailyBudget {
		t.Errorf("a member write moved their document's budgets to %v/%v", mine.AI.DailyBudget, mine.AI.PerUserDailyBudget)
	}
	if mine.AI.ShowCostEstimates == before.AI.ShowCostEstimates {
		t.Error("the rest of the ai section was not written")
	}
	deployment, _ := b.FakeBackend.LoadSettings(ctx)
	if deployment.AI.DailyBudget != 200 {
		t.Errorf("a member write moved the enforced budget to %v", deployment.AI.DailyBudget)
	}
}

// bigFlow has an overview far past any context budget, so the context text is cut at the budget.
func bigFlow() *models.FlowDocument {
	doc := &models.FlowDocument{ID: "big", Name: "Big"}
	for i := range 3000 {
		doc.Subflows = append(doc.Subflows, models.Subflow{ID: fmt.Sprintf("sf%d", i), Name: fmt.Sprintf("Subflow_with_a_reasonably_long_name_%04d", i)})
	}
	doc.RebuildIndexes()
	return doc
}

// TestCachedContextCore_UsesTheCallersContextBudget: a user's own contextTokenBudget was never read in cloud (the
// deployment document, {Version:1} on a fresh database, always gave 4000), and the budget was not in the cache key.
func TestCachedContextCore_UsesTheCallersContextBudget(t *testing.T) {
	b := &countingBackend{FakeBackend: testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1"}}}}
	svc := &ChatService{chatCtxCache: newChatContextCache(), mode: config.ModeCloud, backend: b,
		settings: NewBackendSettingsProvider(context.Background(), b)}
	ctx := auth.WithClaims(context.Background(), &auth.Claims{UserID: "u1"})
	doc := bigFlow()
	withBudget := func(n int) {
		s := models.DefaultSettings()
		s.AI.Providers = map[string]models.AIProviderConfig{"stub": {ContextTokenBudget: n}}
		if err := b.SaveUserSettings(context.Background(), "u1", settingsFromModel(s)); err != nil {
			t.Fatal(err)
		}
	}

	withBudget(8000)
	large := svc.cachedContextCore(ctx, "u1", ctxCacheStubProvider{}, doc, nil, models.ChatRequest{}).contextText
	withBudget(2000)
	small := svc.cachedContextCore(ctx, "u1", ctxCacheStubProvider{}, doc, nil, models.ChatRequest{}).contextText
	if len(large) < 3*len(small) || !strings.Contains(large, "Subflow_with") {
		t.Errorf("context of %d bytes at the user's 8000-token budget vs %d at 2000: the user's budget is not used (or a stale entry was served)",
			len(large), len(small))
	}
}

// TestGetSuggestedPrompts_FreshCloudDatabase: the prompts came from the deployment document, empty on a fresh
// cloud database, so the suggestions were always [].
func TestGetSuggestedPrompts_FreshCloudDatabase(t *testing.T) {
	b := &countingBackend{}
	svc := &ChatService{settings: NewBackendSettingsProvider(context.Background(), b), mode: config.ModeCloud, backend: b}
	got, err := svc.GetSuggestedPrompts(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := models.DefaultSettings().AI.Prompts.Block; len(want) == 0 || len(got) != len(want) {
		t.Errorf("suggested prompts = %q, want the default block prompts %q", got, want)
	}
}
