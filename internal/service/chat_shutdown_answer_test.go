package service

// R-streaming-c16: a stream storing its answer when the stop's budget ends gets its terminal event out before
// Shutdown returns: the server's stop aborts the write, and the worker sends the done it already claimed.

import (
	"context"
	"sync"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	fake "pad-analyzer/internal/testutil"
	"pad-core/models"
)

// blockingAnswerBackend blocks every save after the first (the question's) until its context ends or 5 s pass.
type blockingAnswerBackend struct {
	*fake.FakeBackend
	mu    sync.Mutex
	saves int
}

func (b *blockingAnswerBackend) SaveConversationIf(ctx context.Context, flowID, scope, owner string, expected int64, messages []storageif.ChatMessage) (int64, error) {
	b.mu.Lock()
	b.saves++
	n := b.saves
	b.mu.Unlock()
	if n > 1 {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return b.FakeBackend.SaveConversationIf(ctx, flowID, scope, owner, expected, messages)
}

func terminalEventCount(svc *ChatService, streamID string) int {
	n := 0
	for _, ev := range svc.notifier.(*fake.CountingNotifier).Events() {
		env, ok := ev.Data.(map[string]interface{})
		if ok && ev.Name == "chat:event" && env["streamId"] == streamID && (env["type"] == "done" || env["type"] == "error") {
			n++
		}
	}
	return n
}

func TestShutdown_AbortsALongAnswerWriteAndSendsItsDone(t *testing.T) {
	p := &fake.FakeProvider{Fallback: &fake.FakeTurn{Text: "the answer", TokensIn: 1, TokensOut: 1}}
	backend := &blockingAnswerBackend{FakeBackend: fake.NewFakeBackend()}
	doc := &models.FlowDocument{ID: "f-durable", Name: "f"}
	svc := failedTurnService(t, "shutdown-slow-answer", p, doc)
	svc.backend = backend
	svc.shutdownBudget = 100 * time.Millisecond
	restoreDelay := answerWriteRetryDelay
	answerWriteRetryDelay = time.Millisecond
	t.Cleanup(func() { answerWriteRetryDelay = restoreDelay })

	req := durableRequest("shutdown-slow-answer")
	id, err := svc.StreamChatMessage(context.Background(), "test", doc, nil, req)
	if err == nil {
		svc.BeginStream(context.Background(), id)
	}
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	// Wait until the answer's write is under way.
	deadline := time.Now().Add(5 * time.Second)
	for {
		backend.mu.Lock()
		saves := backend.saves
		backend.mu.Unlock()
		if saves >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the answer write never started")
		}
		time.Sleep(2 * time.Millisecond)
	}
	start := time.Now()
	_ = svc.Shutdown(context.Background())
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Shutdown took %v, want its budget plus a short margin", took)
	}
	if n := terminalEventCount(svc, id); n != 1 {
		t.Errorf("%d terminal events when Shutdown returned, want exactly 1", n)
	}
	// The done says the answer is not stored, and a reattaching client hears the same.
	if done := doneEvent(svc, id); done == nil || done["answerStored"] != false {
		t.Errorf("done = %v, want answerStored false", done)
	}
	if res, err := svc.ResumeStream(context.Background(), id, 0); err != nil || res.AnswerStored == nil || *res.AnswerStored {
		t.Errorf("resume = %+v, %v; want answerStored false", res, err)
	}
}

func TestPersistAnswer_StripeLockGivesUpAtTheStop(t *testing.T) {
	svc := &ChatService{}
	mu := svc.convMutexFor("f", "k")
	mu.Lock()
	defer mu.Unlock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		svc.signalStop()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), persistAnswerTimeout)
	defer cancel()
	start := time.Now()
	if err := svc.lockConversation(ctx, mu); err == nil {
		t.Fatal("the lock was acquired while another writer held it")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("gave up after %v, want right after the stop", took)
	}
}
