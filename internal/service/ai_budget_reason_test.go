package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-core/models"
)

// Phase 4b review T7: the hook's refusal metric (pad_ai_budget_refusals_total{reason}) reads the reason off the
// checker's error; the service's refusals must name theirs, or every spent budget counts as "unavailable".
func TestAIBudget_RefusalNamesItsReason(t *testing.T) {
	budget := NewAIBudget(&staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 5}}}, spendByOrg{"": 9})
	err := budget.Check(context.Background(), ai.BudgetCall{Scope: "u1", ProviderID: "claude", Kind: ai.BudgetCallChat})
	r, ok := err.(ai.BudgetReasoner)
	if !ok {
		t.Fatalf("Check = %T %v, want an ai.BudgetReasoner", err, err)
	}
	if got := r.BudgetReason(); got != "exceeded" {
		t.Errorf("BudgetReason() = %q, want exceeded", got)
	}
	if got := budgetUnavailable("u1", context.DeadlineExceeded).(ai.BudgetReasoner).BudgetReason(); got != "unavailable" {
		t.Errorf("an unavailable check's BudgetReason() = %q, want unavailable", got)
	}
}
