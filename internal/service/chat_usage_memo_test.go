package service

import (
	"context"
	"sync/atomic"
	"testing"

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
