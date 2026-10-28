package service

// Step 8: a turn's question and answer are stored together or not at all, and a history that cannot be read is
// never answered without.
//   - R-tool-loop-19: a history load failure ends the turn with HISTORY_UNAVAILABLE before any provider call.
//   - R-replicas-12 / R-tool-loop-19: a question whose write failed at the start is stored with its answer at the end.
//   - R-tool-loop-20: an answer write that fails says so on done (answerStored false), is retried, and is counted.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/ai"
	storageif "pad-analyzer/internal/storage/interfaces"
	fake "pad-analyzer/internal/testutil"
	"pad-core/models"
)

// faultyConvBackend fails the next loads and saves of a conversation as told.
type faultyConvBackend struct {
	*fake.FakeBackend
	mu        sync.Mutex
	failLoads int
	failSaves []error // one error per save, consumed in order; nil entries succeed
}

func (b *faultyConvBackend) LoadConversationVersioned(ctx context.Context, flowID, scope, owner string) ([]storageif.ChatMessage, int64, error) {
	b.mu.Lock()
	if b.failLoads > 0 {
		b.failLoads--
		b.mu.Unlock()
		return nil, 0, errors.New("simulated: connection reset by peer")
	}
	b.mu.Unlock()
	return b.FakeBackend.LoadConversationVersioned(ctx, flowID, scope, owner)
}

func (b *faultyConvBackend) SaveConversationIf(ctx context.Context, flowID, scope, owner string, expected int64, messages []storageif.ChatMessage) (int64, error) {
	b.mu.Lock()
	if len(b.failSaves) > 0 {
		err := b.failSaves[0]
		b.failSaves = b.failSaves[1:]
		if err != nil {
			b.mu.Unlock()
			return 0, err
		}
	}
	b.mu.Unlock()
	return b.FakeBackend.SaveConversationIf(ctx, flowID, scope, owner, expected, messages)
}

func durabilityService(t *testing.T, provider string, p ai.Provider, backend *faultyConvBackend) *ChatService {
	t.Helper()
	doc := &models.FlowDocument{ID: "f-durable", Name: "f"}
	svc := failedTurnService(t, provider, p, doc)
	svc.backend = backend
	return svc
}

func durableRequest(provider string) models.ChatRequest {
	return models.ChatRequest{Provider: provider, UserMessage: "what does it do?", ConversationID: uuid.NewString(),
		UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString()}
}

func runDurableTurn(t *testing.T, svc *ChatService, req models.ChatRequest) (*streamCtl, string) {
	t.Helper()
	id, err := svc.StreamChatMessage(context.Background(), "test", &models.FlowDocument{ID: "f-durable", Name: "f"}, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.BeginStream(context.Background(), id)
	return waitStreamFinished(t, svc, id), id
}

// assertNoAnswerAfterAnswer is the invariant: the server's own writes never put an assistant message directly
// after another assistant message.
func assertNoAnswerAfterAnswer(t *testing.T, history []models.ChatMessage) {
	t.Helper()
	for i := 1; i < len(history); i++ {
		if history[i].Role == "assistant" && history[i-1].Role == "assistant" {
			t.Errorf("message %d is an answer right after another answer: %+v", i, history)
		}
	}
}

func TestTurnDurability_HistoryLoadFailureFailsTheTurn(t *testing.T) {
	p := &fake.FakeProvider{Fallback: &fake.FakeTurn{Text: "an answer without context"}}
	backend := &faultyConvBackend{FakeBackend: fake.NewFakeBackend(), failLoads: 1}
	svc := durabilityService(t, "durable-load", p, backend)
	ctl, _ := runDurableTurn(t, svc, durableRequest("durable-load"))
	ctl.mu.Lock()
	code := ctl.errCode
	ctl.mu.Unlock()
	if code != ChatErrHistoryUnavailable {
		t.Errorf("code %q, want %s", code, ChatErrHistoryUnavailable)
	}
	if n := p.Calls(); n != 0 {
		t.Errorf("the provider was called %d times for a turn without its history", n)
	}
}

func TestTurnDurability_QuestionLostAtStartIsStoredWithTheAnswer(t *testing.T) {
	for name, startErr := range map[string][]error{
		"every round lost":  {storageif.ErrConversationConflict, storageif.ErrConversationConflict, storageif.ErrConversationConflict, storageif.ErrConversationConflict, storageif.ErrConversationConflict},
		"a transport error": {errors.New("simulated: broken pipe")},
	} {
		t.Run(name, func(t *testing.T) {
			restore := mutateConversationBackoff
			mutateConversationBackoff = func(int) time.Duration { return 0 }
			t.Cleanup(func() { mutateConversationBackoff = restore })
			p := &fake.FakeProvider{Fallback: &fake.FakeTurn{Text: "the answer", TokensIn: 1, TokensOut: 1}}
			backend := &faultyConvBackend{FakeBackend: fake.NewFakeBackend(), failSaves: startErr}
			svc := durabilityService(t, "durable-pair-"+name, p, backend)
			// An earlier exchange is already stored.
			prior := []models.ChatMessage{{ID: uuid.NewString(), Role: "user", Content: "earlier"}, {ID: uuid.NewString(), Role: "assistant", Content: "earlier answer"}}
			req := durableRequest("durable-pair-" + name)
			if _, err := backend.FakeBackend.SaveConversationIf(context.Background(), "f-durable", req.ConversationKey(), "", 0, toStorageMessages(prior)); err != nil {
				t.Fatal(err)
			}
			before := testutil.ToFloat64(chatConversationConflictsTotal.WithLabelValues("answer_with_question"))
			runDurableTurn(t, svc, req)
			history, err := svc.GetConversation(context.Background(), "f-durable", req.ConversationKey())
			if err != nil {
				t.Fatal(err)
			}
			n := len(history)
			if n != 4 || history[n-2].ID != req.UserMessageID || history[n-2].Role != "user" || history[n-1].ID != req.AssistantMessageID {
				t.Fatalf("stored %+v, want the earlier exchange, then the question and its answer", history)
			}
			assertNoAnswerAfterAnswer(t, history)
			if got := testutil.ToFloat64(chatConversationConflictsTotal.WithLabelValues("answer_with_question")); got != before+1 {
				t.Errorf("answer_with_question counted %v, want %v", got, before+1)
			}
		})
	}
}

func TestTurnDurability_FailedAnswerWriteIsSaidAndCounted(t *testing.T) {
	p := &fake.FakeProvider{Fallback: &fake.FakeTurn{Text: "the answer", TokensIn: 1, TokensOut: 1}}
	down := errors.New("simulated: database is restarting")
	// The question's write succeeds; every answer write fails.
	backend := &faultyConvBackend{FakeBackend: fake.NewFakeBackend(), failSaves: []error{nil, down, down, down, down}}
	svc := durabilityService(t, "durable-answer", p, backend)
	restoreDelay := answerWriteRetryDelay
	answerWriteRetryDelay = time.Millisecond
	t.Cleanup(func() { answerWriteRetryDelay = restoreDelay })
	before := testutil.ToFloat64(chatAnswerWriteFailuresTotal.WithLabelValues("error"))
	_, id := runDurableTurn(t, svc, durableRequest("durable-answer"))
	done := doneEvent(svc, id)
	if done == nil || done["answerStored"] != false {
		t.Errorf("done = %v, want answerStored false", done)
	}
	// A client that reattaches after the done learns it too.
	if res, err := svc.ResumeStream(context.Background(), id, 0); err != nil || res.AnswerStored == nil || *res.AnswerStored {
		t.Errorf("resume = %+v, %v; want answerStored false", res, err)
	}
	if snap := svc.finishedCtl(t, id).snapshot(); snap.AnswerStored == nil || *snap.AnswerStored {
		t.Errorf("the mirrored snapshot says answerStored %v, want false", snap.AnswerStored)
	}
	if got := testutil.ToFloat64(chatAnswerWriteFailuresTotal.WithLabelValues("error")); got != before+1 {
		t.Errorf("answer write failures counted %v, want %v", got, before+1)
	}

	// One failure and then a success: retried, stored, versioned.
	backend = &faultyConvBackend{FakeBackend: fake.NewFakeBackend(), failSaves: []error{nil, down}}
	svc = durabilityService(t, "durable-answer-retry", p, backend)
	req := durableRequest("durable-answer-retry")
	_, id = runDurableTurn(t, svc, req)
	done = doneEvent(svc, id)
	if done == nil || done["answerStored"] == false || done["conversationVersion"] == nil {
		t.Errorf("done = %v, want the answer stored and the version stamped", done)
	}
	storedAnswer(t, svc, "f-durable", req)
}

// doneEvent is the data of the stream's done event as the notifier delivered it.
func doneEvent(svc *ChatService, streamID string) map[string]interface{} {
	n, ok := svc.notifier.(*fake.CountingNotifier)
	if !ok {
		return nil
	}
	for _, ev := range n.Events() {
		env, ok := ev.Data.(map[string]interface{})
		if ok && ev.Name == "chat:event" && env["streamId"] == streamID && env["type"] == "done" {
			data, _ := env["data"].(map[string]interface{})
			return data
		}
	}
	return nil
}

// finishedCtl is a finished stream's control.
func (s *ChatService) finishedCtl(t *testing.T, id string) *streamCtl {
	t.Helper()
	v, ok := s.finishedStreams.Load(id)
	if !ok {
		t.Fatalf("stream %s is not in the finished set", id)
	}
	return v.(*streamCtl)
}
