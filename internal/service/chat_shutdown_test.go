package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// The server's stop (ADR 0024): ChatService.Shutdown ends every running stream with SERVER_RESTARTING while the SSE
// connections are still open, waits for the workers (their terminal event, final resume snapshot and a just-finished
// answer's persist) and refuses new streams.

// parkedProvider streams a first chunk and then waits: for the stream's context, or, when ignoreCtx is set, only
// for release (a worker stuck past its cancellation).
type parkedProvider struct {
	*testutil.FakeProvider
	ignoreCtx bool
	release   chan struct{}
	once      sync.Once
}

func newParkedProvider(ignoreCtx bool) *parkedProvider {
	return &parkedProvider{FakeProvider: &testutil.FakeProvider{}, ignoreCtx: ignoreCtx, release: make(chan struct{})}
}

func (p *parkedProvider) Stream(ctx context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
	onChunk(ai.Chunk{Text: "partial"})
	if p.ignoreCtx {
		<-p.release
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
	}
	onChunk(ai.Chunk{Done: true, FinishReason: "stop", TokensIn: 1, TokensOut: 1})
	return nil
}

func (p *parkedProvider) unpark() { p.once.Do(func() { close(p.release) }) }

// shutdownTestService is a local-mode chat service on a counting notifier, with the provider registered as id.
func shutdownTestService(t *testing.T, id string, p ai.Provider) (*ChatService, *testutil.CountingNotifier) {
	t.Helper()
	ai.RegisterProvider(id, func(string) ai.Provider { return p })
	n := &testutil.CountingNotifier{}
	return &ChatService{
		notifier: n, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
	}, n
}

// startLiveStream starts a stream with a client stream id (emission unblocked at once) and waits for its first
// chunk, so the stream is mid-answer.
func startLiveStream(t *testing.T, svc *ChatService, n *testutil.CountingNotifier, provider string) string {
	t.Helper()
	id, err := svc.StreamChatMessage(context.Background(), "u1", &models.FlowDocument{ID: "f-shutdown", Name: "f"}, nil,
		models.ChatRequest{Provider: provider, UserMessage: "hi", ClientStreamID: uuid.NewString()})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if types, _ := eventTypes(n); indexOf(types, "chunk") >= 0 {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream never emitted its first chunk")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// errorCodes lists the codes of the stream's error events, in emission order.
func errorCodes(n *testutil.CountingNotifier, streamID string) []string {
	var codes []string
	for _, ev := range n.Events() {
		env, ok := ev.Data.(map[string]interface{})
		if !ok || ev.Name != "chat:event" || env["streamId"] != streamID || env["type"] != "error" {
			continue
		}
		data, _ := env["data"].(map[string]interface{})
		code, _ := data["code"].(string)
		codes = append(codes, code)
	}
	return codes
}

// A running stream is ended with SERVER_RESTARTING, and its worker has delivered the terminal event and settled by
// the time Shutdown returns: the caller closes the SSE connections right after.
func TestChatService_ShutdownEndsStreamsWithServerRestarting(t *testing.T) {
	p := newParkedProvider(false)
	t.Cleanup(p.unpark)
	svc, n := shutdownTestService(t, "shutdown-parked", p)
	id := startLiveStream(t, svc, n, "shutdown-parked")

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if codes := errorCodes(n, id); len(codes) != 1 || codes[0] != ChatErrServerRestarting {
		t.Errorf("error events %v when Shutdown returned, want one %s", codes, ChatErrServerRestarting)
	}
	v, ok := svc.finishedStreams.Load(id)
	if !ok {
		t.Fatal("the stream is not finished when Shutdown returned")
	}
	select {
	case <-v.(*streamCtl).settled:
	default:
		t.Error("the stream's worker had not settled when Shutdown returned")
	}
	res, err := svc.ResumeStream(context.Background(), id, 0)
	if err != nil || res.ErrorCode != ChatErrServerRestarting || res.Text != "partial" {
		t.Errorf("resume after Shutdown = %+v, %v; want the partial text and %s", res, err, ChatErrServerRestarting)
	}
}

// From Shutdown on, a new stream is refused before anything starts.
func TestChatService_ShutdownRefusesNewStreams(t *testing.T) {
	registerFakeProvider()
	svc, _ := shutdownTestService(t, "shutdown-unused", newParkedProvider(false))
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	_, err := svc.StreamChatMessage(context.Background(), "u1", &models.FlowDocument{ID: "f1", Name: "f"}, nil,
		models.ChatRequest{Provider: "fake", UserMessage: "hi", ClientStreamID: uuid.NewString()})
	if !errors.Is(err, ErrServerRestarting) {
		t.Fatalf("StreamChatMessage after Shutdown = %v, want ErrServerRestarting", err)
	}
	n := 0
	svc.activeStreams.Range(func(any, any) bool { n++; return true })
	if n != 0 {
		t.Errorf("%d streams active after a refused start", n)
	}
}

// slowAnswerBackend stores the turn's answer (its second conversation write) only after a pause: the persist of an
// answer that finished just before the server stopped.
type slowAnswerBackend struct {
	*testutil.FakeBackend
	saves        atomic.Int32
	pause        time.Duration
	answerSaving chan struct{}
}

func (b *slowAnswerBackend) SaveConversation(ctx context.Context, flowID, scope string, messages []storageif.ChatMessage) error {
	if b.saves.Add(1) == 2 {
		close(b.answerSaving)
		time.Sleep(b.pause)
	}
	return b.FakeBackend.SaveConversation(ctx, flowID, scope, messages)
}

// R2: a stream that finished just before the stop is still storing its answer; Shutdown waits for that write (the
// pool closes after the stop hooks), so the answer is in the history when it returns.
func TestChatService_ShutdownWaitsForWorkers(t *testing.T) {
	registerFakeProvider()
	backend := &slowAnswerBackend{FakeBackend: testutil.NewFakeBackend(), pause: 300 * time.Millisecond, answerSaving: make(chan struct{})}
	svc := &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
		backend: backend, mode: config.ModeCloud,
	}
	doc := &models.FlowDocument{ID: "f-shutdown", Name: "f"}
	req := models.ChatRequest{Provider: "fake", UserMessage: "hi", ClientStreamID: uuid.NewString(), AssistantMessageID: uuid.NewString()}
	if _, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, req); err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	select {
	case <-backend.answerSaving:
	case <-time.After(5 * time.Second):
		t.Fatal("the answer was never stored")
	}

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	history, err := svc.GetConversation(context.Background(), doc.ID, req.ConversationKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[1].ID != req.AssistantMessageID {
		t.Fatalf("history when Shutdown returned = %+v, want the question and the stored answer", history)
	}
}

// R2, multi-replica: the final resume snapshot carries SERVER_RESTARTING by the time Shutdown returns (Redis closes
// after the stop hooks), so a reconnect to another replica learns why the stream ended.
func TestChatService_ShutdownPublishesTheFinalSnapshot(t *testing.T) {
	client := newMiniRedis(t)
	p := newParkedProvider(false)
	t.Cleanup(p.unpark)
	svc, n := shutdownTestService(t, "shutdown-mirrored", p)
	svc.resume = redisResumeStore{c: client}
	id := startLiveStream(t, svc, n, "shutdown-mirrored")

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	snap, ok := redisResumeStore{c: client}.Load(context.Background(), id)
	if !ok || snap.ErrorCode != ChatErrServerRestarting || snap.Text != "partial" {
		t.Errorf("snapshot another replica reads after Shutdown = %+v (found %v), want the partial text and %s", snap, ok, ChatErrServerRestarting)
	}
}

// A worker stuck past its cancellation must not hold the stop: at the budget, Shutdown records, emits and mirrors
// the stream's SERVER_RESTARTING itself (the client hears of it before its connection closes) and reports the wait.
func TestChatService_ShutdownBudgetEndsAStuckStream(t *testing.T) {
	p := newParkedProvider(true)
	svc, n := shutdownTestService(t, "shutdown-stuck", p)
	svc.shutdownBudget = 50 * time.Millisecond
	id := startLiveStream(t, svc, n, "shutdown-stuck")
	t.Cleanup(func() {
		p.unpark()
		waitStreamFinished(t, svc, id)
	})

	start := time.Now()
	err := svc.Shutdown(context.Background())
	if err == nil {
		t.Error("Shutdown reported no error with a worker still running")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("Shutdown waited %v, want about its 50ms budget", waited)
	}
	if codes := errorCodes(n, id); len(codes) != 1 || codes[0] != ChatErrServerRestarting {
		t.Errorf("error events %v, want one %s emitted by Shutdown", codes, ChatErrServerRestarting)
	}
	res, rerr := svc.ResumeStream(context.Background(), id, 0)
	if rerr != nil || res.ErrorCode != ChatErrServerRestarting {
		t.Errorf("resume = %+v, %v; want %s", res, rerr, ChatErrServerRestarting)
	}
}
