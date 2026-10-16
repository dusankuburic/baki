package service

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	storageif "pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
	"pad-core/models"
)

// conflicts reads pad_chat_conversation_conflicts_total for each source.
func conflicts() map[string]float64 {
	out := map[string]float64{}
	for _, src := range conversationConflictSources {
		out[src] = testutil.ToFloat64(chatConversationConflictsTotal.WithLabelValues(src))
	}
	return out
}

// conflictDelta is what each source rose by since before.
func conflictDelta(before map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for src, v := range conflicts() {
		if d := v - before[src]; d != 0 {
			out[src] = d
		}
	}
	return out
}

func wantConflicts(t *testing.T, before map[string]float64, want map[string]float64) {
	t.Helper()
	got := conflictDelta(before)
	if len(got) != len(want) {
		t.Fatalf("pad_chat_conversation_conflicts_total rose by %v, want %v", got, want)
	}
	for src, n := range want {
		if got[src] != n {
			t.Fatalf("pad_chat_conversation_conflicts_total rose by %v, want %v", got, want)
		}
	}
}

// Every lost compare-and-swap is counted by where it happened: a client save or clear answered 409, a server write's
// round that is retried, and a server write that gave up.
func TestConversationConflicts_AreCountedBySource(t *testing.T) {
	ctx := context.Background()

	t.Run("client_save", func(t *testing.T) {
		svc := cloudChat(tu.NewFakeBackend())
		saved, err := svc.SaveClientConversation(ctx, "f", "k", new(int64), []models.ChatMessage{{ID: "m1", Role: "user", Content: "one"}})
		if err != nil {
			t.Fatal(err)
		}
		before := conflicts()
		stale := saved.Version - 1
		if _, err := svc.SaveClientConversation(ctx, "f", "k", &stale, []models.ChatMessage{{ID: "m2", Role: "user"}}); err == nil {
			t.Fatal("a stale save was accepted")
		}
		wantConflicts(t, before, map[string]float64{"client_save": 1})
	})

	t.Run("client_clear", func(t *testing.T) {
		svc := cloudChat(tu.NewFakeBackend())
		saved, err := svc.SaveClientConversation(ctx, "f", "k", new(int64), []models.ChatMessage{{ID: "m1", Role: "user", Content: "one"}})
		if err != nil {
			t.Fatal(err)
		}
		before := conflicts()
		if err := svc.ClearConversationIf(ctx, "f", "k", saved.Version-1); err == nil {
			t.Fatal("a stale clear was accepted")
		}
		wantConflicts(t, before, map[string]float64{"client_clear": 1})
	})

	t.Run("retry", func(t *testing.T) {
		noBackoff(t)
		backend := &interleavingBackend{FakeBackend: tu.NewFakeBackend(), times: 2, peer: storageif.ChatMessage{ID: "peer", Role: "user"}}
		svc := cloudChat(backend)
		before := conflicts()
		if stored, err := svc.mutateConversation(ctx, "f", "k", appendMine); err != nil || !stored {
			t.Fatalf("mutateConversation = (%v, %v)", stored, err)
		}
		wantConflicts(t, before, map[string]float64{"retry": 2})
	})

	t.Run("retry_exhausted", func(t *testing.T) {
		noBackoff(t)
		backend := &interleavingBackend{FakeBackend: tu.NewFakeBackend(), times: 1000, peer: storageif.ChatMessage{ID: "peer", Role: "user"}}
		svc := cloudChat(backend)
		before := conflicts()
		if stored, _ := svc.mutateConversation(ctx, "f", "k", appendMine); stored {
			t.Fatal("a write that lost every round was stored")
		}
		wantConflicts(t, before, map[string]float64{"retry": mutateConversationAttempts - 1, "retry_exhausted": 1})
	})
}

func appendMine(h []models.ChatMessage) ([]models.ChatMessage, bool, error) {
	return append(h, models.ChatMessage{ID: "mine", Role: "user", Content: "x"}), true, nil
}
