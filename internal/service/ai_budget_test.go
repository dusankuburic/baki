package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-4: the budget a chat service is built on is the one the knowledge base consults, and it sees the chat
// streams' in-flight spend — a stream enters the ledger when it is admitted, and a stream refused by the
// per-caller cap never does.
func TestAIBudget_SeesTheChatStreamsInFlight(t *testing.T) {
	ctx := context.Background()
	settings := &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 1.0}}}
	store := testutil.NewFakeBackend()
	budget := NewAIBudget(settings, store)
	chat := NewChatService(nil, t.TempDir(), nil, nil, settings, nil, store, config.ModeCloud, ChatOptions{Budget: budget})

	streams := make([]*streamCtl, 0, maxConcurrentStreamsPerScope+1)
	for i := 0; i <= maxConcurrentStreamsPerScope; i++ {
		ctl := &streamCtl{ownerID: "u1", orgID: "org-1"}
		ctl.setInflight(0.4)
		ok, err := chat.tryStartStream(string(rune('a'+i)), "u1", ctl)
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		if ok != (i < maxConcurrentStreamsPerScope) {
			t.Fatalf("stream %d admitted = %v", i, ok)
		}
		streams = append(streams, ctl)
	}
	// Three admitted streams hold $1.20 of the org's $1 budget; the refused fourth holds nothing.
	if err := budget.CheckBudget(ctx, "u2", "org-1"); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Fatalf("org budget with $1.20 in flight: %v, want ErrAIBudgetExceeded", err)
	}
	orgSum, _ := budget.othersInFlight("u2", "org-1", nil)
	if want := 0.4 * float64(maxConcurrentStreamsPerScope); orgSum < want-1e-9 || orgSum > want+1e-9 {
		t.Errorf("in flight = %v, want %v (the refused stream must not count)", orgSum, want)
	}
	for _, ctl := range streams {
		budget.release(ctl)
	}
	if err := budget.CheckBudget(ctx, "u2", "org-1"); err != nil {
		t.Errorf("after the streams ended: %v", err)
	}
	if chat.aiBudget() != budget {
		t.Error("the chat service does not run on the budget it was built with")
	}
}
