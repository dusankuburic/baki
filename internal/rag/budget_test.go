package rag

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// budgetStub is a BudgetChecker that records its calls and refuses with err.
type budgetStub struct {
	err          error
	scope, orgID string
	calls        int
}

func (b *budgetStub) CheckBudget(_ context.Context, scope, orgID string) error {
	b.calls++
	b.scope, b.orgID = scope, orgID
	return b.err
}

var errBudgetSpent = errors.New("daily AI budget exceeded")

// R-4: an upload and a re-index are billed embedding runs; a spent daily
// budget refuses them before a single embedding call, with the checker's
// own error (the HTTP layer renders it 429 BUDGET_EXCEEDED).
func TestKnowledge_BudgetGatesUploadAndReindex(t *testing.T) {
	store := testutil.NewFakeBackend()
	seedCorpus(store, []float32{1, 0}, "existing guideline")
	emb := &fixedEmbedder{vec: []float32{1, 0}}
	svc := guardService(store, emb)
	budget := &budgetStub{err: errBudgetSpent}
	svc.SetBudgetChecker(budget)

	if err := svc.AddDocument(context.Background(), "u1", "org-1", "new.md", "some content"); !errors.Is(err, errBudgetSpent) {
		t.Errorf("upload over budget: %v, want the budget refusal", err)
	}
	if budget.scope != "u1" || budget.orgID != "org-1" {
		t.Errorf("budget checked for %q/%q, want u1/org-1", budget.scope, budget.orgID)
	}
	if _, err := svc.ReindexOrg(context.Background(), "u1", "org-1", false, interfaces.KnowledgeChunkCursor{}); !errors.Is(err, errBudgetSpent) {
		t.Errorf("re-index over budget: %v, want the budget refusal", err)
	}
	if emb.calls != 0 {
		t.Errorf("%d embedding calls were made over budget", emb.calls)
	}

	budget.err = nil
	if err := svc.AddDocument(context.Background(), "u1", "org-1", "new.md", "some content"); err != nil {
		t.Errorf("upload within budget: %v", err)
	}
	if emb.calls == 0 {
		t.Error("an upload within budget embedded nothing")
	}
	if budget.calls != 3 {
		t.Errorf("budget consulted %d times, want once per run (3)", budget.calls)
	}
}
