package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// Usage rows land asynchronously, so a budget check that only reads the store
// lets N streams started together all pass the same read — count live streams.
func TestEnforceBudget_ConcurrentStreamsShareTheBudget(t *testing.T) {
	svc := &ChatService{
		backend:  &testutil.FakeBackend{DailyUsage: 0},
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 1.0}}},
	}
	a := &streamCtl{ownerID: "user-1", orgID: "org-1"}
	a.setInflight(0.6)
	b := &streamCtl{ownerID: "user-2", orgID: "org-1"}
	b.setInflight(0.6)
	svc.activeStreams.Store("a", a)
	svc.activeStreams.Store("b", b)
	ctx := context.Background()

	// A third org member: $0 persisted, but $1.20 already committed by the
	// two live streams — must trip.
	if err := svc.enforceBudget(ctx, "user-3", "org-1", 0, nil); err == nil {
		t.Fatal("two live streams already over the org budget; a third must be refused")
	}
	// The same check from stream a itself excludes its own entry: only b's
	// $0.60 counts, so a may continue (0.6 < 1.0).
	if err := svc.enforceBudget(ctx, "user-1", "org-1", 0, a); err != nil {
		t.Fatalf("a stream must not double-count its own ledger entry: %v", err)
	}
	// Another org's streams are not this org's spend.
	if err := svc.enforceBudget(ctx, "user-9", "org-2", 0, nil); err != nil {
		t.Fatalf("org-2 has nothing in flight: %v", err)
	}
	// Personal scope (no org) counts only the same user's streams.
	if err := svc.enforceBudget(ctx, "user-1", "", 0, nil); err != nil {
		t.Fatalf("personal budget must count user-1's own streams only (0.6 < 1.0): %v", err)
	}
	a.setInflight(0.5)
	c := &streamCtl{ownerID: "user-1", orgID: ""}
	c.setInflight(0.5)
	svc.activeStreams.Store("c", c)
	if err := svc.enforceBudget(ctx, "user-1", "", 0, nil); err == nil {
		t.Fatal("two of user-1's own streams at $1.00 in flight must trip the personal budget")
	}
}

// The per-user sub-limit inside an org counts the user's other streams too.
func TestEnforceBudget_PerUserSubLimitCountsConcurrent(t *testing.T) {
	svc := &ChatService{
		backend:  &testutil.FakeBackend{DailyUsage: 0},
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 10, PerUserDailyBudget: 1.0}}},
	}
	other := &streamCtl{ownerID: "user-1", orgID: "org-1"}
	other.setInflight(0.9)
	svc.activeStreams.Store("o", other)
	if err := svc.enforceBudget(context.Background(), "user-1", "org-1", 0.2, nil); err == nil {
		t.Fatal("user-1 has $0.90 in flight elsewhere; $0.20 more must trip the $1 per-user limit")
	}
}
