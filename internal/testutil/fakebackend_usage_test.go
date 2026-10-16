package testutil

import (
	"context"
	"sync"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// SaveUsageMetric keeps every row it receives — including ServedModel (D-42)
// — as an independent copy, so a test can assert what the audited provider
// recorded even when the caller reuses or mutates its struct afterwards.
func TestFakeBackend_SaveUsageMetricKeepsServedModel(t *testing.T) {
	be := &FakeBackend{}
	ctx := context.Background()

	m := interfaces.UsageMetric{ID: "u-1", UserID: "user-1", Provider: "azure-foundry", Model: "model-router", ServedModel: "gpt-4o", PromptTokens: 10, CompletionTokens: 2, EstimatedCost: 0.01}
	if err := be.SaveUsageMetric(ctx, &m); err != nil {
		t.Fatalf("SaveUsageMetric: %v", err)
	}
	m.ServedModel = "mutated-after-save"
	if err := be.SaveUsageMetric(ctx, nil); err != nil {
		t.Fatalf("SaveUsageMetric(nil): %v", err)
	}

	got := be.SavedUsageMetrics()
	if len(got) != 1 {
		t.Fatalf("saved %d rows, want 1 (nil rows are ignored)", len(got))
	}
	if got[0].Model != "model-router" || got[0].ServedModel != "gpt-4o" {
		t.Errorf("row = %+v, want Model=model-router ServedModel=gpt-4o (a copy, not the caller's struct)", got[0])
	}
	if got[0].EstimatedCost != 0.01 || got[0].PromptTokens != 10 || got[0].CompletionTokens != 2 {
		t.Errorf("row lost its usage fields: %+v", got[0])
	}
}

// The audited provider records from detached goroutines; concurrent saves
// must all land (run with -race to prove the mutex earns its keep).
func TestFakeBackend_SaveUsageMetricConcurrent(t *testing.T) {
	be := &FakeBackend{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			served := ""
			if i%2 == 0 {
				served = "gpt-4o"
			}
			_ = be.SaveUsageMetric(context.Background(), &interfaces.UsageMetric{ID: "x", Model: "deploy", ServedModel: served})
		}(i)
	}
	wg.Wait()
	got := be.SavedUsageMetrics()
	if len(got) != 50 {
		t.Fatalf("saved %d rows, want 50", len(got))
	}
	var withServed int
	for _, r := range got {
		if r.ServedModel == "gpt-4o" {
			withServed++
		}
	}
	if withServed != 25 {
		t.Errorf("%d rows carry a served model, want 25", withServed)
	}
}

// DeleteUser keeps the erased user's organisation spend as the organisation's (without the user) and deletes their
// personal spend, as Postgres's erasure does (lifecycle_users.go).
func TestFakeBackend_DeleteUserKeepsOrgSpend(t *testing.T) {
	ctx := context.Background()
	be := NewFakeBackend()
	for _, m := range []*interfaces.UsageMetric{
		{ID: "org", UserID: "gone", OrgID: "o1", EstimatedCost: 1.5},
		{ID: "own", UserID: "gone", EstimatedCost: 0.7},
		{ID: "other", UserID: "stays", OrgID: "o1", EstimatedCost: 2},
	} {
		if err := be.SaveUsageMetric(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := be.DeleteUser(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range be.SavedUsageMetrics() {
		got[r.ID] = r.UserID + "|" + r.OrgID
	}
	want := map[string]string{"org": "|o1", "other": "stays|o1"}
	if len(got) != len(want) || got["org"] != want["org"] || got["other"] != want["other"] {
		t.Errorf("usage rows after erasure = %v, want %v (organisation spend kept without the user, personal spend gone)", got, want)
	}
}
