package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// waitClosed fails the test unless ch closes within 5 s.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// --- a stream is reported only once its question is stored ---------------------

// questionGateProvider parks the turn in prepareTurn — its model listing
// (the output-limit lookup) blocks until models is closed — and then in
// Stream until release, closing streaming when the stream begins.
type questionGateProvider struct {
	*testutil.FakeProvider
	models    chan struct{}
	streaming chan struct{}
	release   chan struct{}
}

func (p *questionGateProvider) Models(ctx context.Context) ([]ai.ModelInfo, error) {
	select {
	case <-p.models:
	case <-ctx.Done():
	}
	return nil, nil
}

func (p *questionGateProvider) Stream(ctx context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
	close(p.streaming)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	onChunk(ai.Chunk{Text: "the answer"})
	onChunk(ai.Chunk{Done: true, FinishReason: "stop", TokensIn: 3, TokensOut: 2})
	return nil
}

// The stream is registered before prepareTurn runs, but its question is
// stored only after it. A reload in between used to re-attach with a history
// lacking the question, and the client's full-replace save on done erased the
// question the server had stored meanwhile.
func TestActiveStream_ReportedOnlyOnceTheQuestionIsStored(t *testing.T) {
	p := &questionGateProvider{FakeProvider: &testutil.FakeProvider{}, models: make(chan struct{}), streaming: make(chan struct{}), release: make(chan struct{})}
	ai.RegisterProvider("review4-question-gate", func(string) ai.Provider { return p })
	svc := &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
	}
	t.Cleanup(svc.CancelAll) // a failed assertion must not leave the turn parked
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}
	const conv = "11111111-2222-4333-8444-555555555555"
	ctx := context.Background()
	id, err := svc.StreamChatMessage(ctx, "u1", doc, nil, models.ChatRequest{
		Provider: "review4-question-gate", Model: "fake-model", UserMessage: "the question",
		ConversationID: conv, ClientStreamID: "99999999-9999-4999-8999-999999999991",
	})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	if _, live := svc.activeStreams.Load(id); !live {
		t.Fatal("the stream is not registered")
	}
	if got := svc.ActiveStreamFor("u1", "f1", conv); got != "" {
		t.Errorf("ActiveStreamFor while prepareTurn runs = %q, want empty (the question is not stored yet)", got)
	}
	if msgs, sid, err := svc.ConversationWithActiveStream(ctx, "u1", doc.ID, conv); err != nil || sid != "" || len(msgs) != 0 {
		t.Errorf("GET before the question is stored = (%d messages, %q, %v), want no messages and no stream", len(msgs), sid, err)
	}

	close(p.models)
	waitClosed(t, p.streaming, "the provider stream")
	msgs, sid, err := svc.ConversationWithActiveStream(ctx, "u1", doc.ID, conv)
	if err != nil {
		t.Fatal(err)
	}
	if sid != id {
		t.Errorf("activeStreamId once the question is stored = %q, want %q", sid, id)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "the question" {
		t.Errorf("history reported with the stream = %+v, want the stored question", msgs)
	}

	close(p.release)
	waitStreamFinished(t, svc, id)
	msgs, sid, err = svc.ConversationWithActiveStream(ctx, "u1", doc.ID, conv)
	if err != nil {
		t.Fatal(err)
	}
	if sid != "" || len(msgs) != 2 || msgs[1].Content != "the answer" {
		t.Errorf("after the stream settled = (%+v, %q), want the stored answer and no stream", msgs, sid)
	}
}

// The start-of-stream persist reports whether the question was stored: with
// "Save conversation history" off it is accepted and dropped, so the stream
// must not be reported either.
func TestPersistInitialUserTurn_ReportsWhetherStored(t *testing.T) {
	ctx := context.Background()
	doc := &models.FlowDocument{ID: "f1"}
	req := models.ChatRequest{UserMessage: "q", ConversationID: "11111111-2222-4333-8444-555555555555"}
	withHistory := func(on bool) *ChatService {
		return &ChatService{configDir: t.TempDir(), settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{SaveConversationHistory: on}}}}
	}
	if !withHistory(true).persistInitialUserTurn(ctx, doc, req) {
		t.Error("history on: persistInitialUserTurn = false, want true")
	}
	if withHistory(false).persistInitialUserTurn(ctx, doc, req) {
		t.Error("history off: persistInitialUserTurn = true for a dropped save, want false")
	}
	if (&ChatService{configDir: t.TempDir()}).persistInitialUserTurn(ctx, doc, models.ChatRequest{}) {
		t.Error("no question: persistInitialUserTurn = true, want false")
	}
}

// --- a finished stream is reported while its answer is being stored ------------

// answerGateStore is a conversation store whose second save — the turn's
// answer, after its question — parks until release (saving closes as it
// starts), so a test can read the conversation in that window.
type answerGateStore struct {
	*testutil.FakeBackend
	mu      sync.Mutex
	saves   int
	saving  chan struct{}
	release chan struct{}
}

func (s *answerGateStore) SaveConversation(ctx context.Context, flowID, scope string, msgs []storageif.ChatMessage) error {
	s.mu.Lock()
	s.saves++
	answer := s.saves == 2
	s.mu.Unlock()
	if answer {
		close(s.saving)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.FakeBackend.SaveConversation(ctx, flowID, scope, msgs)
}

func (s *answerGateStore) LoadConversation(ctx context.Context, flowID, scope string) ([]storageif.ChatMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.FakeBackend.LoadConversation(ctx, flowID, scope)
}

// The cleanup takes a finished stream out of the active set before it stores
// the answer (up to persistAnswerTimeout under the conversation mutex): a
// reload in that window saw neither the stream nor the answer. The finished
// stream is reported until it settles.
func TestActiveStream_ReportedWhileTheAnswerIsStored(t *testing.T) {
	store := &answerGateStore{FakeBackend: testutil.NewFakeBackend(), saving: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	releaseStore := func() { once.Do(func() { close(store.release) }) }
	t.Cleanup(releaseStore)
	ai.RegisterProvider("review4-answer", func(string) ai.Provider {
		return &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "the answer", TokensIn: 3, TokensOut: 2}}}
	})
	svc := &ChatService{
		notifier: &testutil.CountingNotifier{}, flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
		mode: config.ModeCloud, backend: store,
	}
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}
	const conv = "11111111-2222-4333-8444-555555555555"
	ctx := context.Background()
	id, err := svc.StreamChatMessage(ctx, "u1", doc, nil, models.ChatRequest{
		Provider: "review4-answer", Model: "fake-model", UserMessage: "the question",
		ConversationID: conv, ClientStreamID: "99999999-9999-4999-8999-999999999992",
	})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	waitClosed(t, store.saving, "the answer's save")
	if _, live := svc.activeStreams.Load(id); live {
		t.Fatal("the stream is still live while its answer is stored")
	}
	msgs, sid, err := svc.ConversationWithActiveStream(ctx, "u1", doc.ID, conv)
	if err != nil {
		t.Fatal(err)
	}
	if sid != id {
		t.Errorf("activeStreamId while the answer is stored = %q, want %q (the history does not carry it yet)", sid, id)
	}
	if len(msgs) != 1 || msgs[0].Content != "the question" {
		t.Errorf("history = %+v, want the question alone", msgs)
	}

	releaseStore()
	waitStreamFinished(t, svc, id)
	msgs, sid, err = svc.ConversationWithActiveStream(ctx, "u1", doc.ID, conv)
	if err != nil {
		t.Fatal(err)
	}
	if sid != "" || len(msgs) != 2 || msgs[1].Content != "the answer" {
		t.Errorf("after the stream settled = (%+v, %q), want the stored answer and no stream", msgs, sid)
	}
}

// A GET whose history read lands after the answer was stored but before the
// stream settled must not report the stream: the client would show — and
// save — the answer twice. Same last-message test persistAssistantTurn
// de-dupes the client's own save with.
func TestConversationWithActiveStream_DropsAStreamWhoseAnswerIsStored(t *testing.T) {
	ctx := context.Background()
	svc := &ChatService{configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "f1"}
	ctl := &streamCtl{ownerID: "u1", flowID: "f1", convKey: "c1", streamID: "s1", settled: make(chan struct{}), done: true}
	ctl.userTurnStored.Store(true)
	ctl.buffer.WriteString("the answer")
	svc.finishedStreams.Store("s1", ctl)
	question := models.ChatMessage{ID: "m1", Role: "user", Content: "the question"}

	if err := svc.SaveConversation(ctx, doc.ID, "c1", []models.ChatMessage{question}); err != nil {
		t.Fatal(err)
	}
	if _, sid, err := svc.ConversationWithActiveStream(ctx, "u1", doc.ID, "c1"); err != nil || sid != "s1" {
		t.Errorf("answer not stored yet: sid = %q (%v), want s1", sid, err)
	}
	if err := svc.SaveConversation(ctx, doc.ID, "c1", []models.ChatMessage{question, {ID: "m2", Role: "assistant", Content: "the answer"}}); err != nil {
		t.Fatal(err)
	}
	if _, sid, err := svc.ConversationWithActiveStream(ctx, "u1", doc.ID, "c1"); err != nil || sid != "" {
		t.Errorf("answer stored: sid = %q (%v), want none", sid, err)
	}
	if got := svc.ActiveStreamFor("u1", "f1", "c1"); got != "s1" {
		t.Errorf("ActiveStreamFor = %q, want s1 (unsettled; only the history read tells it is stored)", got)
	}
}

// --- Azure AI Foundry is never refused by the daily budget ---------------------

// exhaustedBudget is a ChatService whose daily budget ($1) is already spent
// ($5) by priced providers.
func exhaustedBudget(t *testing.T) *ChatService {
	return &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), chatCtxCache: newChatContextCache(),
		backend:  &testutil.FakeBackend{DailyUsage: 5},
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 1, SaveConversationHistory: true}}},
	}
}

// lastEvent is the last captured event (the terminal one).
func lastEvent(t *testing.T, evs []capturedEvent) capturedEvent {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	return evs[len(evs)-1]
}

// Plain stream path: prepareTurn's budget check.
func TestStream_AzureTurnIgnoresAnExhaustedBudget(t *testing.T) {
	for _, tc := range []struct {
		id      string
		refused bool
	}{
		{ai.AzureFoundryProviderID, false},
		{"openai", true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			name := "review4-budget-" + tc.id
			ai.RegisterProvider(name, func(string) ai.Provider {
				return &aliasPricedProvider{
					FakeProvider: &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 3, TokensOut: 2}}},
					id:           tc.id,
					price:        ai.Pricing{InputCostPerM: 1, OutputCostPerM: 1},
				}
			})
			svc := exhaustedBudget(t)
			doc := &models.FlowDocument{ID: "f1", Name: "f1"}
			id, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, models.ChatRequest{
				Provider: name, Model: "dep", UserMessage: "hi", ClientStreamID: "99999999-9999-4999-8999-999999999993",
			})
			if err != nil {
				t.Fatalf("StreamChatMessage: %v", err)
			}
			ctl := waitStreamFinished(t, svc, id)
			types, payloads := eventTypes(svc.notifier.(*testutil.CountingNotifier))
			if len(types) == 0 {
				t.Fatal("no events")
			}
			last, data := types[len(types)-1], payloads[len(payloads)-1]
			if tc.refused {
				if last != "error" || data["code"] != ChatErrBudgetExceeded {
					t.Errorf("events %v, last %v: want the priced turn refused with %s", types, data, ChatErrBudgetExceeded)
				}
				return
			}
			if last != "done" {
				t.Errorf("events %v, last %v: want the Azure turn answered despite the spent budget", types, data)
			}
			ctl.mu.Lock()
			answer := ctl.buffer.String()
			ctl.mu.Unlock()
			if answer != "answer" {
				t.Errorf("answer = %q", answer)
			}
		})
	}
}

// Both tool loops re-check the budget every iteration; an Azure turn passes
// every check (a tool call, then the final answer), a priced one is refused
// at the first.
func TestToolLoops_AzureIgnoresAnExhaustedBudget(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		id      string
		refused bool
	}{
		{ai.AzureFoundryProviderID, false},
		{"openai", true},
	} {
		for _, loop := range []string{"native", "prompt"} {
			t.Run(tc.id+"/"+loop, func(t *testing.T) {
				turns := []testutil.FakeTurn{
					{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: []byte(`{"query":"xav"}`)}}, TokensIn: 10, TokensOut: 5},
					{Text: "Final answer", TokensIn: 8, TokensOut: 3},
				}
				if loop == "prompt" {
					turns = []testutil.FakeTurn{{Text: "Final answer", TokensIn: 8, TokensOut: 3}}
				}
				p := &aliasPricedProvider{FakeProvider: &testutil.FakeProvider{Turns: turns}, id: tc.id, price: ai.Pricing{InputCostPerM: 1, OutputCostPerM: 1}}
				svc := exhaustedBudget(t)
				ctl := &streamCtl{}
				emit, events := captureEvents()
				req := ai.Request{Model: "dep", Messages: []ai.Message{{Role: "user", Content: "hi"}}}
				tctx := &ai.ToolContext{Ctx: ctx, Doc: toolLoopDoc()}
				if loop == "prompt" {
					svc.runPromptToolLoop(ctx, p, req, "user-1", tctx, ctl, func() bool { return true }, emit, func() int { return 0 })
				} else {
					svc.runToolLoop(ctx, p, req, "user-1", tctx, ctl, func() bool { return true }, emit, func() int { return 0 })
				}
				last := lastEvent(t, events())
				if tc.refused {
					if last.typ != "error" || last.data["code"] != ChatErrBudgetExceeded {
						t.Errorf("last event %+v, want the priced loop refused with %s", last, ChatErrBudgetExceeded)
					}
					if p.Calls() != 0 {
						t.Errorf("%d provider calls over budget, want 0", p.Calls())
					}
					return
				}
				if last.typ != "done" {
					t.Errorf("last event %+v, want the Azure loop answered despite the spent budget", last)
				}
				if p.Calls() != len(turns) {
					t.Errorf("provider calls = %d, want %d (every iteration ran)", p.Calls(), len(turns))
				}
			})
		}
	}
}

// --- a provider status line is a trail row, not a hosted step -------------------

// noticeProvider streams a provider status line (the "reasoning off" notice)
// twice — RetryingProvider forwards a failed attempt's notice as well as the
// retry's — before each scripted turn; afterNotice runs once the notice
// chunks were handled.
type noticeProvider struct {
	*testutil.FakeProvider
	afterNotice func()
}

func (p *noticeProvider) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	notice := ai.Chunk{Notice: &ai.Notice{Name: "reasoning_off", Label: "Reasoning is off for this deployment"}}
	onChunk(notice)
	onChunk(notice)
	if p.afterNotice != nil {
		p.afterNotice()
	}
	return p.FakeProvider.Stream(ctx, req, onChunk)
}

// checkNoticeEvents asserts exactly one "tool" event carrying the notice for
// the whole turn (however many provider calls and retries raised it), no
// tool_result for it, and a done event after it.
func checkNoticeEvents(t *testing.T, types []string, payloads []map[string]interface{}) {
	t.Helper()
	rows, results, first := 0, 0, -1
	for i, typ := range types {
		if payloads[i]["name"] != "reasoning_off" {
			continue
		}
		switch typ {
		case "tool":
			rows++
			if first < 0 {
				first = i
			}
			if payloads[i]["label"] != "Reasoning is off for this deployment" {
				t.Errorf("tool event = %v, want the notice's label", payloads[i])
			}
		case "tool_result":
			results++
		}
	}
	if rows != 1 || results != 0 {
		t.Errorf("events %v: want exactly one reasoning_off tool event and no tool_result", types)
	}
	if done := indexOf(types, "done"); first < 0 || done < first {
		t.Errorf("events %v: want the notice row before done", types)
	}
}

func TestStream_ProviderNoticeIsATrailRow(t *testing.T) {
	t.Run("plain stream path", func(t *testing.T) {
		svc := &ChatService{
			notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
			factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
		}
		hosted := int32(-1)
		p := &noticeProvider{FakeProvider: &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 3, TokensOut: 2}}}}
		p.afterNotice = func() {
			svc.activeStreams.Range(func(_, v interface{}) bool {
				hosted = v.(*streamCtl).hostedOpen.Load()
				return false
			})
		}
		ai.RegisterProvider("review4-notice", func(string) ai.Provider { return p })
		id, err := svc.StreamChatMessage(context.Background(), "u1", &models.FlowDocument{ID: "f1", Name: "f1"}, nil, models.ChatRequest{
			Provider: "review4-notice", Model: "fake-model", UserMessage: "hi", ClientStreamID: "99999999-9999-4999-8999-999999999994",
		})
		if err != nil {
			t.Fatalf("StreamChatMessage: %v", err)
		}
		waitStreamFinished(t, svc, id)
		if hosted != 0 {
			t.Errorf("hosted steps open after the notice = %d, want 0 (the idle limit stays the normal one)", hosted)
		}
		types, payloads := eventTypes(svc.notifier.(*testutil.CountingNotifier))
		checkNoticeEvents(t, types, payloads)
	})

	for _, loop := range []string{"native", "prompt"} {
		t.Run(loop+" tool loop", func(t *testing.T) {
			ctx := context.Background()
			svc := &ChatService{analysisCache: &AnalysisService{}}
			ctl := &streamCtl{}
			hosted := int32(-1)
			// The native loop runs two provider calls (a flow tool, then the
			// answer), each raising the notice: still one row.
			turns := []testutil.FakeTurn{
				{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: []byte(`{"query":"xav"}`)}}, TokensIn: 10, TokensOut: 5},
				{Text: "answer", TokensIn: 3, TokensOut: 2},
			}
			if loop == "prompt" {
				turns = turns[1:]
			}
			p := &noticeProvider{FakeProvider: &testutil.FakeProvider{Turns: turns}, afterNotice: func() { hosted = ctl.hostedOpen.Load() }}
			emit, events := captureEvents()
			req := ai.Request{Messages: []ai.Message{{Role: "user", Content: "hi"}}}
			tctx := &ai.ToolContext{Ctx: ctx, Doc: toolLoopDoc()}
			if loop == "prompt" {
				svc.runPromptToolLoop(ctx, p, req, "user-1", tctx, ctl, func() bool { return true }, emit, func() int { return 0 })
			} else {
				svc.runToolLoop(ctx, p, req, "user-1", tctx, ctl, func() bool { return true }, emit, func() int { return 0 })
			}
			if hosted != 0 {
				t.Errorf("hosted steps open after the notice = %d, want 0", hosted)
			}
			if p.Calls() != len(turns) {
				t.Fatalf("provider calls = %d, want %d", p.Calls(), len(turns))
			}
			var types []string
			var payloads []map[string]interface{}
			for _, ev := range events() {
				types = append(types, ev.typ)
				payloads = append(payloads, ev.data)
			}
			checkNoticeEvents(t, types, payloads)
		})
	}
}
