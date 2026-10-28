package service

// Step 8 review round: a stopped start is a stop, not a history failure; a panicking answer write still ends the
// stream with its done; a conversation cleared while its stream runs is not written again by that stream.

import (
	"context"
	"sync"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	fake "pad-analyzer/internal/testutil"
	"pad-core/models"
)

// parkedLoadBackend parks the conversation's load until the caller's context ends.
type parkedLoadBackend struct {
	*fake.FakeBackend
	loading chan struct{}
	once    sync.Once
}

func (b *parkedLoadBackend) LoadConversationVersioned(ctx context.Context, flowID, scope, owner string) ([]storageif.ChatMessage, int64, error) {
	b.once.Do(func() { close(b.loading) })
	<-ctx.Done()
	return nil, 0, ctx.Err()
}

func TestTurnReview_AStopDuringTheHistoryLoadIsAStop(t *testing.T) {
	p := &fake.FakeProvider{Fallback: &fake.FakeTurn{Text: "unused"}}
	backend := &parkedLoadBackend{FakeBackend: fake.NewFakeBackend(), loading: make(chan struct{})}
	svc := failedTurnService(t, "review-stop-load", p, &models.FlowDocument{ID: "f-durable", Name: "f"})
	svc.backend = backend
	id, err := svc.StreamChatMessage(context.Background(), "test", &models.FlowDocument{ID: "f-durable", Name: "f"}, nil, durableRequest("review-stop-load"))
	if err != nil {
		t.Fatal(err)
	}
	svc.BeginStream(context.Background(), id)
	select {
	case <-backend.loading:
	case <-time.After(5 * time.Second):
		t.Fatal("the history load never started")
	}
	svc.CancelStream(id)
	ctl := waitStreamFinished(t, svc, id)
	ctl.mu.Lock()
	code := ctl.errCode
	ctl.mu.Unlock()
	if code != ChatErrStopped {
		t.Errorf("code %q, want %s: the user stopped the turn", code, ChatErrStopped)
	}
}

// panickingAnswerBackend panics on the answer's write (the turn's second conversation write).
type panickingAnswerBackend struct {
	*fake.FakeBackend
	mu    sync.Mutex
	saves int
}

func (b *panickingAnswerBackend) SaveConversationIf(ctx context.Context, flowID, scope, owner string, expected int64, messages []storageif.ChatMessage) (int64, error) {
	b.mu.Lock()
	b.saves++
	n := b.saves
	b.mu.Unlock()
	if n == 2 {
		panic("simulated: a bug in the store")
	}
	return b.FakeBackend.SaveConversationIf(ctx, flowID, scope, owner, expected, messages)
}

func TestTurnReview_APanickingAnswerWriteStillSendsTheDone(t *testing.T) {
	p := &fake.FakeProvider{Fallback: &fake.FakeTurn{Text: "the answer", TokensIn: 1, TokensOut: 1}}
	svc := failedTurnService(t, "review-panic-write", p, &models.FlowDocument{ID: "f-durable", Name: "f"})
	svc.backend = &panickingAnswerBackend{FakeBackend: fake.NewFakeBackend()}
	_, id := runDurableTurn(t, svc, durableRequest("review-panic-write"))
	if n := terminalEventCount(svc, id); n != 1 {
		t.Fatalf("%d terminal events, want 1", n)
	}
	if done := doneEvent(svc, id); done == nil || done["answerStored"] != false {
		t.Errorf("done = %v, want answerStored false", done)
	}
}

func TestTurnReview_AClearedConversationIsNotWrittenAgainByItsStream(t *testing.T) {
	p := &fake.FakeProvider{Fallback: &fake.FakeTurn{Text: "partial", Block: true}, HonourCtx: true}
	svc := failedTurnService(t, "review-clear-inflight", p, &models.FlowDocument{ID: "f-durable", Name: "f"})
	req := durableRequest("review-clear-inflight")
	id, err := svc.StreamChatMessage(context.Background(), "test", &models.FlowDocument{ID: "f-durable", Name: "f"}, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	svc.BeginStream(context.Background(), id)
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, ok := svc.activeStreams.Load(id)
		if ok && v.(*streamCtl).userTurnStored.Load() && v.(*streamCtl).snapshot().Text != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream never stored its question and streamed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := svc.ClearConversation(context.Background(), "f-durable", req.ConversationKey(), true); err != nil {
		t.Fatal(err)
	}
	svc.CancelStream(id)
	waitStreamFinished(t, svc, id)

	history, err := svc.GetConversation(context.Background(), "f-durable", req.ConversationKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Errorf("the cleared conversation holds %+v after its stream ended, want nothing", history)
	}
}
