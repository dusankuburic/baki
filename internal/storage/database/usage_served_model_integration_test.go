package database_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

// TestUsageServedModel_Postgres covers D-42 end to end at the storage layer:
// SaveUsageMetric persists served_model (migration v22) and the analytics
// model breakdown groups by the model that ANSWERED, so an Azure AI Foundry
// deployment alias routed to two models over the window is two rows, each
// still labelled with the deployment it came through, while a row whose
// provider reported nothing groups by its requested model with an empty
// ServedModel. Gated on DATABASE_URL (the podman harness).
func TestUsageServedModel_Postgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping usage served_model integration test")
	}
	ctx := context.Background()
	b, err := database.New(ctx, database.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	user := fmt.Sprintf("served-model-user-%d", time.Now().UnixNano())
	db := b.DB()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM usage_metrics WHERE user_id = $1`, user)
	})

	now := time.Now()
	rows := []interfaces.UsageMetric{
		{ID: user + "-1", UserID: user, Provider: "azure-foundry", Model: "model-router", ServedModel: "gpt-4o", PromptTokens: 100, CompletionTokens: 10, CreatedAt: now},
		{ID: user + "-2", UserID: user, Provider: "azure-foundry", Model: "model-router", ServedModel: "gpt-4o", PromptTokens: 100, CompletionTokens: 10, CreatedAt: now},
		{ID: user + "-3", UserID: user, Provider: "azure-foundry", Model: "model-router", ServedModel: "gpt-4.1-mini", PromptTokens: 50, CompletionTokens: 5, CreatedAt: now},
		{ID: user + "-4", UserID: user, Provider: "openai", Model: "gpt-4o", PromptTokens: 20, CompletionTokens: 2, CreatedAt: now},
	}
	for i := range rows {
		if err := b.SaveUsageMetric(ctx, &rows[i]); err != nil {
			t.Fatalf("SaveUsageMetric %s: %v", rows[i].ID, err)
		}
	}

	var stored string
	if err := db.QueryRowContext(ctx, `SELECT served_model FROM usage_metrics WHERE id = $1`, user+"-3").Scan(&stored); err != nil {
		t.Fatalf("read served_model: %v", err)
	}
	if stored != "gpt-4.1-mini" {
		t.Errorf("served_model = %q, want %q", stored, "gpt-4.1-mini")
	}

	got, err := b.LoadUsageAnalytics(ctx, user, nil, "user", interfaces.AnalyticsWindow{
		From: now.Add(-time.Hour), To: now.Add(time.Hour), TZ: "UTC",
	})
	if err != nil {
		t.Fatalf("LoadUsageAnalytics: %v", err)
	}
	type key struct{ provider, model, served string }
	byKey := map[key]interfaces.UsageModelRow{}
	for _, r := range got.ByModel {
		byKey[key{r.Provider, r.Model, r.ServedModel}] = r
	}
	if len(byKey) != 3 {
		t.Fatalf("ByModel = %+v, want 3 rows (router→gpt-4o, router→gpt-4.1-mini, openai gpt-4o)", got.ByModel)
	}
	if r := byKey[key{"azure-foundry", "model-router", "gpt-4o"}]; r.Calls != 2 || r.TokensIn != 200 {
		t.Errorf("router→gpt-4o row = %+v, want 2 calls / 200 tokens in", r)
	}
	if r := byKey[key{"azure-foundry", "model-router", "gpt-4.1-mini"}]; r.Calls != 1 || r.TokensIn != 50 {
		t.Errorf("router→gpt-4.1-mini row = %+v, want 1 call / 50 tokens in", r)
	}
	if r, ok := byKey[key{"openai", "gpt-4o", ""}]; !ok || r.Calls != 1 {
		t.Errorf("a row without a served model must group by its requested model with ServedModel empty: %+v", got.ByModel)
	}
}
