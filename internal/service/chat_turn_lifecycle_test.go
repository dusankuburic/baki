package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// waitStreamFinished polls until the stream has left the active set, waits
// for its cleanup to settle (the detached answer persist included — D-4 runs
// it after the stream left the active set, so "no longer active" does not
// mean "answer stored") and returns its control from the finished set.
func waitStreamFinished(t *testing.T, svc *ChatService, id string) *streamCtl {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if v, ok := svc.finishedStreams.Load(id); ok {
			ctl := v.(*streamCtl)
			select {
			case <-ctl.settled:
			case <-deadline:
				t.Fatalf("stream %s did not settle within 5s", id)
			}
			return ctl
		}
		select {
		case <-deadline:
			t.Fatalf("stream %s did not finish within 5s", id)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// keyedFactory resolves every provider with a fixed key (no network).
func keyedFactory() *ai.ProviderFactory {
	return ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
}

// --- D-3: orgID is fixed before the control is published ------------------------

// --- D-21: the reservation is published before the turn-start budget check ------

// --- D-22: a refused turn never starts the (billed) RAG embedding ----------------

// --- D-38: an alias provider reserves nothing, whatever it reports -------------

// --- D-2: truncation pins the CURRENT turn ---------------------------------------

func TestTruncateForContextWindow_KeepsCurrentTurn(t *testing.T) {
	// FakeProvider.EstimateTokens = len/4: a 400-byte message costs 104 tokens
	// (100 + 4 framing); the byte ceiling never short-circuits these.
	provider := &testutil.FakeProvider{}
	msg := func(role, tag string) ai.Message {
		return ai.Message{Role: role, Content: tag + strings.Repeat(".", 400-len(tag))}
	}
	roles := func(ms []ai.Message) string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Role+":"+strings.TrimRight(m.Content, "."))
		}
		return strings.Join(out, ",")
	}

	t.Run("older history goes before the current question", func(t *testing.T) {
		req := &ai.Request{Messages: []ai.Message{msg("user", "old"), msg("assistant", "a0"), msg("user", "current")}}
		pin, err := truncateForContextWindow(context.Background(), provider, req, contextReserve+110, 2) // fits the current turn only
		if err != nil {
			t.Fatalf("truncate: %v", err)
		}
		if got := roles(req.Messages); got != "user:current" {
			t.Errorf("kept %s, want only the current turn (the old form kept user:old and dropped the question)", got)
		}
		if pin != 0 {
			t.Errorf("returned pin = %d, want 0", pin)
		}
	})

	t.Run("tool exchange after the turn survives older history", func(t *testing.T) {
		req := &ai.Request{Messages: []ai.Message{
			msg("user", "old"), msg("assistant", "a0"), msg("user", "current"),
			{Role: "assistant", ToolCalls: []ai.ToolCall{{ID: "c1", Name: "search_flow", Input: []byte(strings.Repeat("x", 400))}}},
			msg("tool", "result"),
		}}
		pin, err := truncateForContextWindow(context.Background(), provider, req, contextReserve+420, 2) // one history turn too many
		if err != nil {
			t.Fatalf("truncate: %v", err)
		}
		if got := roles(req.Messages); got != "user:current,assistant:,tool:result" {
			t.Errorf("kept %s, want the current turn plus its tool exchange", got)
		}
		if pin != 0 {
			t.Errorf("returned pin = %d, want 0", pin)
		}
	})

	t.Run("prompt-loop exchanges drop as one unit after the turn", func(t *testing.T) {
		req := &ai.Request{Messages: []ai.Message{
			msg("user", "current"), msg("assistant", "A1"), msg("user", "<tool_result>1"), msg("assistant", "A2"), msg("user", "<tool_result>2"),
		}}
		pin, err := truncateForContextWindow(context.Background(), provider, req, contextReserve+420, 0)
		if err != nil {
			t.Fatalf("truncate: %v", err)
		}
		if got := roles(req.Messages); got != "user:current,assistant:A2,user:<tool_result>2" {
			t.Errorf("kept %s, want the oldest exchange (A1 + its result) dropped together", got)
		}
		if pin != 0 {
			t.Errorf("returned pin = %d, want 0", pin)
		}
	})

	t.Run("the current turn is never dropped", func(t *testing.T) {
		req := &ai.Request{Messages: []ai.Message{msg("user", "old"), msg("assistant", "a0"), msg("user", "current")}}
		if _, err := truncateForContextWindow(context.Background(), provider, req, contextReserve+50, 2); !errors.Is(err, ai.ErrContextLimit) {
			t.Errorf("err = %v, want ErrContextLimit when the current turn alone overflows", err)
		}
	})

	t.Run("a pin that moved is reported", func(t *testing.T) {
		req := &ai.Request{Messages: []ai.Message{msg("user", "old"), msg("assistant", "a0"), msg("user", "u1"), msg("assistant", "a1"), msg("user", "current"), msg("assistant", "post"), msg("tool", "r")}}
		pin, err := truncateForContextWindow(context.Background(), provider, req, contextReserve+104*5+10, 4)
		if err != nil {
			t.Fatalf("truncate: %v", err)
		}
		if got := roles(req.Messages); got != "user:u1,assistant:a1,user:current,assistant:post,tool:r" {
			t.Errorf("kept %s", got)
		}
		if pin != 2 || req.Messages[pin].Content != msg("user", "current").Content {
			t.Errorf("returned pin = %d, want 2 (the current turn's new index)", pin)
		}
	})

	t.Run("MaxTokens raises the reserve", func(t *testing.T) {
		// 312 prompt tokens fit an 8110 window against the default 4000
		// reserve; against the request's own 8000-token completion cap only
		// the current turn does.
		req := &ai.Request{MaxTokens: 8000, Messages: []ai.Message{msg("user", "old"), msg("assistant", "a0"), msg("user", "current")}}
		if _, err := truncateForContextWindow(context.Background(), provider, req, 8000+110, 2); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		if len(req.Messages) != 1 {
			t.Errorf("a completion cap above the default reserve must be reserved too: kept %d messages, want 1", len(req.Messages))
		}
	})
}

// countingEstimator counts tokenizer calls.
type countingEstimator struct {
	*testutil.FakeProvider
	calls int
}

func (c *countingEstimator) EstimateTokens(t string) int {
	c.calls++
	return c.FakeProvider.EstimateTokens(t)
}

// A prompt whose BYTE count fits the window fits in tokens (every
// estimator yields at most one token per byte), so the tokenizer pass is
// skipped entirely.
func TestTruncateForContextWindow_SkipsTokenizerWhenBytesFit(t *testing.T) {
	p := &countingEstimator{FakeProvider: &testutil.FakeProvider{}}
	req := &ai.Request{
		SystemPrompt: strings.Repeat("s", 1000),
		Tools:        []ai.ToolDefinition{{Name: "t", InputSchema: []byte(strings.Repeat("{}", 100))}},
		Messages:     []ai.Message{{Role: "user", Content: strings.Repeat("u", 2000)}, {Role: "assistant", Content: "a", ToolCalls: []ai.ToolCall{{Input: []byte("{}")}}}, {Role: "user", Content: "q"}},
	}
	if got, want := promptTokenCeiling(req), 1000+200+8+2000+4+1+4+2+1+4; got != want {
		t.Fatalf("promptTokenCeiling = %d, want %d", got, want)
	}
	if _, err := truncateForContextWindow(context.Background(), p, req, promptTokenCeiling(req)+contextReserve, 2); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Errorf("tokenizer called %d times for a prompt that fits in bytes, want 0", p.calls)
	}
	if len(req.Messages) != 3 {
		t.Errorf("messages truncated: %d, want 3", len(req.Messages))
	}
	// One byte over the ceiling and the real estimate runs (and fits).
	if _, err := truncateForContextWindow(context.Background(), p, req, promptTokenCeiling(req)+contextReserve-1, 2); err != nil {
		t.Fatal(err)
	}
	if p.calls == 0 {
		t.Error("tokenizer must run once the byte ceiling does not prove the prompt fits")
	}

	// A replayed provider record (ProviderItems) is re-sent verbatim,
	// so the byte ceiling and the estimate both bound a record-bearing
	// message by its record. A tiny-content turn carrying a ~200 KB record
	// no longer slips under a limit sized for the content alone: the
	// tokenizer pass runs and the post-pin exchange carrying the record is
	// dropped.
	t.Run("a replay record is counted", func(t *testing.T) {
		p := &countingEstimator{FakeProvider: &testutil.FakeProvider{}}
		record := json.RawMessage(strings.Repeat("r", 200_000))
		req := &ai.Request{Messages: []ai.Message{
			{Role: "user", Content: "q"}, // the pin
			{Role: "assistant", Content: "a", ProviderItems: record, ProviderItemsFrom: "claude"},
			{Role: "tool", Content: "r"},
		}}
		legacy := len("q") + 4 + len("a") + 4 + len("r") + 4 // content-only ceiling
		if _, err := truncateForContextWindow(context.Background(), p, req, legacy+contextReserve, 0); err != nil {
			t.Fatal(err)
		}
		if p.calls == 0 {
			t.Fatal("the tokenizer pass must run: the record pushes the real ceiling far past the content-only limit")
		}
		if len(req.Messages) != 1 || req.Messages[0].Content != "q" {
			t.Errorf("kept %d messages, want only the pinned turn (the record-bearing exchange dropped)", len(req.Messages))
		}
		// The per-message ceiling is the record's bytes + framing.
		solo := &ai.Request{Messages: []ai.Message{{Role: "assistant", ProviderItems: record}}}
		if got, want := promptTokenCeiling(solo), len(record)+4; got != want {
			t.Errorf("promptTokenCeiling of a record-only message = %d, want %d", got, want)
		}
	})
}

// --- D-4: the detached answer write is bounded ----------------------------------

// hangingConversationStore blocks LoadConversation until the context ends.
type hangingConversationStore struct {
	*testutil.FakeBackend
}

func (h *hangingConversationStore) LoadConversation(ctx context.Context, _, _ string) ([]storageif.ChatMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (h *hangingConversationStore) LoadConversationVersioned(ctx context.Context, _, _ string) ([]storageif.ChatMessage, int64, error) {
	<-ctx.Done()
	return nil, 0, ctx.Err()
}

func TestPersistAssistantTurn_ReturnsWhenStoreHangs(t *testing.T) {
	prev := persistAnswerTimeout
	persistAnswerTimeout = 50 * time.Millisecond
	t.Cleanup(func() { persistAnswerTimeout = prev })

	svc := &ChatService{mode: config.ModeCloud, backend: &hangingConversationStore{FakeBackend: testutil.NewFakeBackend()}}
	doc := &models.FlowDocument{ID: "flow-1"}
	req := models.ChatRequest{ConversationID: "11111111-1111-4111-8111-111111111111"}
	ctl := &streamCtl{done: true}
	ctl.buffer.WriteString("the answer")

	started := time.Now()
	svc.persistAssistantTurn(context.Background(), doc, req, ctl, req.Model, "")
	if took := time.Since(started); took > time.Second {
		t.Fatalf("persistAssistantTurn took %v against a hung store, want the persistAnswerTimeout bound", took)
	}
	if !svc.convMutexFor(doc.ID, req.ConversationKey()).TryLock() {
		t.Error("the conversation stripe is still held after the bounded write returned")
	}
}

// --- D-5: the Redis mirror publishes the terminal state --------------------------

// blockingProvider streams nothing until its context ends.
type blockingProvider struct{ *testutil.FakeProvider }

func (p *blockingProvider) Stream(ctx context.Context, _ ai.Request, _ func(ai.Chunk)) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestMirrorStream_CancelledStreamPublishesTerminalError(t *testing.T) {
	ai.RegisterProvider("blocking-fake", func(string) ai.Provider { return &blockingProvider{FakeProvider: &testutil.FakeProvider{}} })
	client := newMiniRedis(t)
	svc := &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
		resume: redisResumeStore{c: client},
	}
	doc := &models.FlowDocument{ID: "f1"}
	id, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, models.ChatRequest{Provider: "blocking-fake", Model: "fake-model", UserMessage: "hi", ClientStreamID: "12345678-1234-5678-1234-56781234567a"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	// Let the worker reach the provider call, then stop it.
	time.Sleep(50 * time.Millisecond)
	svc.CancelStream(id)
	waitStreamFinished(t, svc, id)

	deadline := time.After(3 * time.Second)
	for {
		snap, ok := svc.resume.Load(context.Background(), id)
		if ok && snap.Error != "" {
			if snap.ErrorCode != ChatErrStopped {
				t.Errorf("backplane snapshot = %+v, want the STREAM_STOPPED terminal state", snap)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("backplane never received the terminal snapshot: %+v (ok=%v)", snap, ok)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// --- D-20: a stream cancelled before its begin gate opens still ends terminal -----

func TestStream_LegacyCancelledBeforeBegin_RecordsTerminalState(t *testing.T) {
	// The fake ignores ctx: its chunks are dropped at the begin gate and it
	// returns nil, so nothing on the worker's own paths records a state.
	ai.RegisterProvider("legacy-cancel-fake", func(string) ai.Provider {
		return &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "x", TokensIn: 1, TokensOut: 1}}}
	})
	svc := &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
	}
	doc := &models.FlowDocument{ID: "f1"}
	id, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, models.ChatRequest{Provider: "legacy-cancel-fake", Model: "fake-model", UserMessage: "hi"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.CancelStream(id) // before /chat/begin
	ctl := waitStreamFinished(t, svc, id)
	snap := ctl.snapshot()
	if snap.Error == "" || snap.ErrorCode != ChatErrStopped {
		t.Fatalf("finished stream has no terminal state: %+v (want the stop recorded)", snap)
	}
	res := svc.BeginStream(context.Background(), id)
	if res == nil || res.Error == "" || res.ErrorCode != ChatErrStopped {
		t.Errorf("BeginStream after the cancel = %+v, want the stored terminal state (a nil result reports the stream as live)", res)
	}
}

// --- D-13: the done payload carries the served model and its rate -----------------

// servedFakeProvider is a provider whose terminal chunk names the model that answered.
type servedFakeProvider struct {
	*testutil.FakeProvider
	served string
	tools  bool
}

func (p *servedFakeProvider) ID() string          { return "served-fake" }
func (p *servedFakeProvider) SupportsTools() bool { return p.tools }
func (p *servedFakeProvider) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	return p.FakeProvider.Stream(ctx, req, func(c ai.Chunk) {
		if c.Done {
			c.ServedModel = p.served
		}
		onChunk(c)
	})
}

func TestStreamChatMessage_DonePayloadCarriesServedModel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		useTools bool
	}{{"plain stream path", false}, {"tool loop path", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := &servedFakeProvider{
				FakeProvider: &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 7, TokensOut: 3}}},
				served:       "gpt-4o-2024-11-20", tools: tc.useTools,
			}
			ai.RegisterProvider("served-fake", func(string) ai.Provider { return p })
			notifier := &testutil.CountingNotifier{}
			svc := &ChatService{
				notifier: notifier, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
				factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
			}
			doc := &models.FlowDocument{ID: "f1"}
			id, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, models.ChatRequest{Provider: "served-fake", Model: "dep", UserMessage: "hi", UseTools: tc.useTools, ClientStreamID: "12345678-1234-5678-1234-56781234567b"})
			if err != nil {
				t.Fatalf("StreamChatMessage: %v", err)
			}
			ctl := waitStreamFinished(t, svc, id)
			types, payloads := eventTypes(notifier)
			var done map[string]interface{}
			for i, typ := range types {
				if typ == "done" {
					done = payloads[i]
				}
			}
			if done == nil {
				t.Fatalf("no done event: %v", types)
			}
			if done["servedModel"] != "gpt-4o-2024-11-20" || done["tokensIn"] != 7 || done["tokensOut"] != 3 {
				t.Errorf("done payload = %v, want servedModel gpt-4o-2024-11-20 with 7/3 tokens", done)
			}
			for _, key := range []string{"inputCostPerM", "outputCostPerM", "cost"} {
				if _, ok := done[key]; ok {
					t.Errorf("done payload carries %s: %v", key, done)
				}
			}
			if snap := ctl.snapshot(); snap.ServedModel != "gpt-4o-2024-11-20" {
				t.Errorf("snapshot.ServedModel = %q", snap.ServedModel)
			}
			res, err := svc.ResumeStream(context.Background(), id, 0)
			if err != nil || res.ServedModel != "gpt-4o-2024-11-20" {
				t.Errorf("ResumeStream served model = %+v (%v)", res, err)
			}
		})
	}
	// A provider that names no served model keeps the plain payload.
	plain := doneEventData("", 1, 2, 3)
	if _, ok := plain["servedModel"]; ok {
		t.Error("servedModel must be omitted when the provider named none")
	}
}

func TestResumeSnapshot_RoundTripsServedModel(t *testing.T) {
	in := resumeSnapshot{Owner: "u1", Text: "t", Done: true, TokensIn: 1, TokensOut: 2, ServedModel: "gpt-4o-2024-11-20"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out resumeSnapshot
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ServedModel != in.ServedModel {
		t.Errorf("round trip lost the served model: %+v", out)
	}
	if b, _ := json.Marshal(resumeSnapshot{}); strings.Contains(string(b), "servedModel") {
		t.Errorf("an empty served model must be omitted: %s", b)
	}
	if b, _ := json.Marshal(ResumeResult{ServedModel: "m"}); !strings.Contains(string(b), `"servedModel":"m"`) {
		t.Errorf("ResumeResult must carry servedModel: %s", b)
	}
}
