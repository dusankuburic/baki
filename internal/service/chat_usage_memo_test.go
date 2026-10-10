package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// countingUsageStore counts GetDailyUsage reads so the per-loop usage memo
// can be observed without depending on its internals.
type countingUsageStore struct {
	*testutil.FakeBackend
	reads atomic.Int32
}

func (c *countingUsageStore) GetDailyUsage(ctx context.Context, scope, orgID string) (float64, error) {
	c.reads.Add(1)
	return c.FakeBackend.GetDailyUsage(ctx, scope, orgID)
}

// TestRecheckBudget_LoopReadsUsageOnce: the tool loop re-checks the budget on
// every iteration, and each store read is a SUM over usage_metrics — the
// loop's memo shares one read across its iterations, while callers outside
// the loop (CheckBudget, the turn-start check) still read fresh, so a
// same-second store mutation stays visible to them.
func TestRecheckBudget_LoopReadsUsageOnce(t *testing.T) {
	store := &countingUsageStore{FakeBackend: testutil.NewFakeBackend()}
	svc := &ChatService{
		backend:  store,
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 10}}},
	}
	ctx := context.Background()
	st := &toolLoopState{s: svc, ctx: ctx, scope: "user-1", orgID: "org-1", ctl: &streamCtl{}}

	for i := 0; i < 5; i++ {
		if !st.recheckBudget() {
			t.Fatalf("recheckBudget iteration %d refused an under-budget turn", i)
		}
	}
	if got := store.reads.Load(); got != 1 {
		t.Fatalf("GetDailyUsage called %d times for 5 loop iterations, want 1", got)
	}
	// Outside the loop there is no memo: a fresh check reads the store again.
	if err := svc.aiBudget().CheckBudget(ctx, "user-1", "org-1"); err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if got := store.reads.Load(); got != 2 {
		t.Fatalf("GetDailyUsage after CheckBudget: %d reads, want 2", got)
	}
	// And a store mutation is visible immediately to that fresh path.
	store.DailyUsage = 50
	if err := svc.aiBudget().CheckBudget(ctx, "user-1", "org-1"); err == nil {
		store.reads.Load() // keep the counter meaningful in failure output
		t.Fatal("CheckBudget must see a just-written over-budget spend")
	}
}

// memoHookProvider streams one answer per call (the hook in the audited wrapper runs before each).
type memoHookProvider struct{ *testutil.FakeProvider }

func (p *memoHookProvider) ID() string { return "memo-hook-stub" }
func (p *memoHookProvider) PricePerMillionTokens() ai.Pricing {
	return ai.Pricing{InputCostPerM: 1, OutputCostPerM: 1}
}
func (p *memoHookProvider) Stream(_ context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
	onChunk(ai.Chunk{Text: "ok"})
	onChunk(ai.Chunk{Done: true, TokensIn: 2, TokensOut: 1})
	return nil
}

// ADR 0028: the wrapper's pre-call hook and the loop's re-checks read today's spend through ONE memo, the
// turn's (ai.WithBudgetMemo on the stream context): a loop of N iterations, each a re-check plus a Stream
// through the hook, still costs one GetDailyUsage.
func TestRecheckBudget_HookAndLoopShareOneUsageRead(t *testing.T) {
	ai.RegisterProvider("memo-hook-stub", func(string) ai.Provider { return &memoHookProvider{FakeProvider: &testutil.FakeProvider{}} })
	store := &countingUsageStore{FakeBackend: testutil.NewFakeBackend()}
	svc := &ChatService{
		backend:  store,
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 10}}},
	}
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "k", nil }, nil, nil, nil, ai.WithBudgetChecker(svc.aiBudget()))
	ctl := &streamCtl{ownerID: "user-1", orgID: "org-1"}
	ctx := ai.WithBudgetScope(ai.WithBudgetMemo(withStreamSpend(context.Background(), ctl)), "user-1", "org-1")
	provider, err := factory.ForOrg(ctx, "user-1", "org-1", "memo-hook-stub")
	if err != nil {
		t.Fatal(err)
	}
	st := &toolLoopState{s: svc, ctx: ctx, provider: provider, scope: "user-1", orgID: "org-1", ctl: ctl}

	for i := 0; i < 4; i++ {
		if !st.recheckBudget() {
			t.Fatalf("recheckBudget iteration %d refused an under-budget turn", i)
		}
		if err := provider.Stream(ctx, ai.Request{Model: "m", OrgID: "org-1"}, func(ai.Chunk) {}); err != nil {
			t.Fatalf("Stream iteration %d: %v", i, err)
		}
	}
	if got := store.reads.Load(); got != 1 {
		t.Fatalf("GetDailyUsage called %d times for 4 re-checks and 4 hooked streams, want 1", got)
	}
	// The hook refuses on the same memo once the loop's own check would: the stream's own entry in the ledger
	// is excluded (withStreamSpend), another stream's counts.
	other := &streamCtl{ownerID: "user-2", orgID: "org-1"}
	other.setInflight(20)
	svc.aiBudget().hold(other)
	if err := provider.Stream(ctx, ai.Request{Model: "m", OrgID: "org-1"}, func(ai.Chunk) {}); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Errorf("hooked stream with $20 in flight on another stream: %v, want ErrAIBudgetExceeded", err)
	}
	if got := store.reads.Load(); got != 1 {
		t.Errorf("GetDailyUsage called %d times, want the memo's one", got)
	}
}
