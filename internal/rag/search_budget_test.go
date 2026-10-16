package rag

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// countingEmbedProvider is a raw embedding provider counting its Embed calls (one per batch).
type countingEmbedProvider struct {
	ai.Provider
	id    string
	mu    sync.Mutex
	calls int
}

func (p *countingEmbedProvider) ID() string                  { return p.id }
func (p *countingEmbedProvider) DefaultModel() string        { return "emb" }
func (p *countingEmbedProvider) EstimateTokens(t string) int { return len(t) / 4 }
func (p *countingEmbedProvider) PricePerMillionTokens() ai.Pricing {
	return ai.Pricing{InputCostPerM: 1, OutputCostPerM: 1}
}
func (p *countingEmbedProvider) Models(context.Context) ([]ai.ModelInfo, error) { return nil, nil }
func (p *countingEmbedProvider) Embed(_ context.Context, text []string) ([][]float32, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	out := make([][]float32, len(text))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (p *countingEmbedProvider) embeds() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// memoBudget stands in for service.AIBudget on both sides of the knowledge base — the run gate (CheckBudget)
// and the wrapper's hook (Check) — reading today's spend through the context's memo and counting the reads.
type memoBudget struct {
	spent  float64
	limit  float64
	mu     sync.Mutex
	reads  int
	checks int
}

func (b *memoBudget) read(ctx context.Context, scope, orgID string) error {
	b.mu.Lock()
	b.checks++
	b.mu.Unlock()
	usage, _ := ai.BudgetMemoFrom(ctx).Usage(scope, orgID, func() (float64, error) {
		b.mu.Lock()
		b.reads++
		b.mu.Unlock()
		return b.spent, nil
	})
	if usage >= b.limit {
		return errBudgetSpent
	}
	return nil
}

func (b *memoBudget) CheckBudget(ctx context.Context, scope, orgID string) error {
	return b.read(ctx, scope, orgID)
}

func (b *memoBudget) Check(ctx context.Context, call ai.BudgetCall) error {
	return b.read(ctx, call.Scope, call.OrgID)
}

// budgetedKnowledge builds the knowledge base on a factory whose chains consult budget, embedding with the
// counting provider through the factory (so the wrapper's hook is in the path, unlike WithEmbedder).
func budgetedKnowledge(t *testing.T, store *testutil.FakeBackend, budget *memoBudget) (*KnowledgeService, *countingEmbedProvider) {
	t.Helper()
	emb := &countingEmbedProvider{id: "rag-budget-stub"}
	ai.RegisterProvider(emb.id, func(string) ai.Provider { return emb })
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "k", nil }, nil, nil, nil, ai.WithBudgetChecker(budget))
	svc := NewKnowledgeService(store, factory, nil, WithEmbeddingProvider(emb.id), WithBudgetChecker(budget))
	return svc, emb
}

// ADR 0028: the per-turn query embed of Search is gated explicitly — and, below it, by the wrapper's hook —
// so a spent budget refuses the search before any embedding call, with the budget's own error.
func TestKnowledge_SearchQueryEmbedIsRefusedOverBudget(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewFakeBackend()
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedCorpus(store, []float32{1, 0}, "existing guideline")
	budget := &memoBudget{spent: 9, limit: 5}
	svc, emb := budgetedKnowledge(t, store, budget)

	if _, err := svc.Search(ctx, "u1", "org-1", "how do I name variables"); !errors.Is(err, errBudgetSpent) {
		t.Errorf("search over budget: %v, want the budget refusal", err)
	}
	if emb.embeds() != 0 {
		t.Errorf("%d embedding calls were made over budget", emb.embeds())
	}
	// The hook alone still refuses when the run gate is absent (a service built without a checker).
	svc.budget = nil
	if _, err := svc.Search(ctx, "u1", "org-1", "how do I name variables"); !errors.Is(err, errBudgetSpent) {
		t.Errorf("search over budget through the hook alone: %v, want the budget refusal", err)
	}
	if emb.embeds() != 0 {
		t.Errorf("%d embedding calls were made over budget (hook)", emb.embeds())
	}
	budget.spent = 1
	if _, err := svc.Search(ctx, "u1", "org-1", "how do I name variables"); err != nil {
		t.Errorf("search within budget: %v", err)
	}
	if emb.embeds() != 1 {
		t.Errorf("search within budget embedded %d times, want 1", emb.embeds())
	}
}

// A billed run reads today's spend ONCE: the run gate and the hook before each of its N batches share the
// run's memo (budgetRun), and every check is charged to the caller in the org.
func TestKnowledge_RunReadsUsageOncePerRun(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewFakeBackend()
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	budget := &memoBudget{spent: 1, limit: 5}
	svc, emb := budgetedKnowledge(t, store, budget)

	// 120 paragraphs of ~900 runes: more than one embedding batch (embedBatchSize 100) and well under the cap.
	para := strings.Repeat("rule ", 180)
	content := strings.Repeat(para+"\n\n", 120)
	if err := svc.AddDocument(ctx, "u1", "org-1", "rules.md", content); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if emb.embeds() < 2 {
		t.Fatalf("the upload embedded in %d call(s), want at least 2 batches for the test to mean anything", emb.embeds())
	}
	if budget.reads != 1 {
		t.Errorf("GetDailyUsage read %d times for a run of %d batches (+ the run gate), want 1", budget.reads, emb.embeds())
	}
	if budget.checks != emb.embeds()+1 {
		t.Errorf("budget checked %d times, want the run gate plus one per batch (%d)", budget.checks, emb.embeds()+1)
	}

	// A re-index is its own run: one more read for its page.
	before := emb.embeds()
	if _, err := svc.ReindexOrg(ctx, "u1", "org-1", true, interfaces.KnowledgeChunkCursor{}); err != nil {
		t.Fatalf("ReindexOrg: %v", err)
	}
	if emb.embeds() <= before {
		t.Fatal("the forced re-index embedded nothing")
	}
	if budget.reads != 2 {
		t.Errorf("GetDailyUsage read %d times after a second run, want 2", budget.reads)
	}
}
