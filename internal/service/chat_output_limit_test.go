package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// scriptedChunkProvider streams one scripted chunk sequence per Stream call —
// finish reasons, reasoning and keepalive chunks included, which FakeTurn
// cannot express.
type scriptedChunkProvider struct {
	*testutil.FakeProvider
	turns [][]ai.Chunk
	tools bool
	calls int
}

func (p *scriptedChunkProvider) SupportsTools() bool { return p.tools }

func (p *scriptedChunkProvider) Stream(_ context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
	i := p.calls
	p.calls++
	if i < len(p.turns) {
		for _, c := range p.turns[i] {
			onChunk(c)
		}
	}
	return nil
}

// runScriptedStream drives a real StreamChatMessage (factory, audited wrapper
// and all) against the scripted provider and returns the chat events plus the
// usage metrics the audited wrapper recorded.
func runScriptedStream(t *testing.T, p *scriptedChunkProvider, useTools bool) (types []string, payloads []map[string]interface{}, usage <-chan *interfaces.UsageMetric) {
	t.Helper()
	_, _, types, payloads, usage = runScriptedStreamCtl(t, p, useTools)
	return types, payloads, usage
}

// runScriptedStreamCtl is runScriptedStream plus the service and stream id,
// for callers that also check the resumable state (F-15).
func runScriptedStreamCtl(t *testing.T, p *scriptedChunkProvider, useTools bool) (svc *ChatService, streamID string, types []string, payloads []map[string]interface{}, usage <-chan *interfaces.UsageMetric) {
	t.Helper()
	// One registry entry, re-pointed per run (subtests run sequentially).
	const id = "scripted-fake"
	ai.RegisterProvider(id, func(string) ai.Provider { return p })
	recorded := make(chan *interfaces.UsageMetric, 8)
	notifier := &testutil.CountingNotifier{}
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil,
		func(_ context.Context, m *interfaces.UsageMetric) error {
			recorded <- m
			return nil
		}, nil)
	svc = &ChatService{
		notifier:      notifier,
		configDir:     t.TempDir(),
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
		settings:      nilSettings{},
		chatCtxCache:  newChatContextCache(),
	}
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}
	streamID, err := svc.StreamChatMessage(context.Background(), "test", doc, nil, models.ChatRequest{
		Provider: id, Model: "reasoner", UserMessage: "analyse this flow", UseTools: useTools,
	})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.BeginStream(context.Background(), streamID)
	// Wait until the stream has SETTLED, not merely left activeStreams: the
	// answer is persisted after that, and returning earlier let the test's
	// TempDir cleanup race the write ("directory is not empty" on Windows).
	waitStreamFinished(t, svc, streamID)
	types, payloads = eventTypes(notifier)
	return svc, streamID, types, payloads, recorded
}

var chatPaths = []struct {
	name     string
	useTools bool
	native   bool // provider supports native function calling
}{
	{"plain stream path", false, true},
	{"tool loop", true, true},
	{"prompt tool loop", true, false},
}

// TestStream_EmptyAnswerAtTokenCapIsOutputLimitError: a reasoning model that
// spends the whole Max tokens budget thinking ends with finish reason
// "length" and no answer. That used to be a successful, billed, empty reply;
// every chat path now fails it with PROVIDER_OUTPUT_LIMIT and no done event,
// while the reported usage is still recorded.
func TestStream_EmptyAnswerAtTokenCapIsOutputLimitError(t *testing.T) {
	for _, path := range chatPaths {
		t.Run(path.name, func(t *testing.T) {
			p := &scriptedChunkProvider{FakeProvider: &testutil.FakeProvider{}, tools: path.native, turns: [][]ai.Chunk{{
				{Reasoning: "the user wants a full analysis, so first"}, // split-off <think> text
				{}, // keepalive
				{Done: true, FinishReason: "length", TokensIn: 50, TokensOut: 4096},
			}}}
			svc, streamID, types, payloads, usage := runScriptedStreamCtl(t, p, path.useTools)

			errAt := indexOf(types, "error")
			if errAt < 0 {
				t.Fatalf("no error event: %v", types)
			}
			if indexOf(types, "done") >= 0 || indexOf(types, "chunk") >= 0 {
				t.Errorf("an empty cut-off answer must not stream or finish as a reply: %v", types)
			}
			if payloads[errAt]["code"] != ChatErrOutputLimit || payloads[errAt]["message"] != outputLimitMsg {
				t.Errorf("error event = %v", payloads[errAt])
			}
			// F-15: the failure is billed, so the error event carries the
			// same usage keys a done event would (minus chunks), and a client
			// that missed the live event gets them from the resume snapshot
			// instead of 0/0.
			if payloads[errAt]["tokensIn"] != 50 || payloads[errAt]["tokensOut"] != 4096 {
				t.Errorf("error event usage = %v/%v, want the reported 50/4096", payloads[errAt]["tokensIn"], payloads[errAt]["tokensOut"])
			}
			res, err := svc.ResumeStream(context.Background(), streamID, 0)
			if err != nil || res.TokensOut != 4096 || res.TokensIn != 50 || res.ErrorCode != ChatErrOutputLimit {
				t.Errorf("resume snapshot = %+v (%v), want the billed 50/4096 with the error code", res, err)
			}
			select {
			case m := <-usage:
				if m.PromptTokens != 50 || m.CompletionTokens != 4096 {
					t.Errorf("recorded usage = %d/%d, want the reported 50/4096", m.PromptTokens, m.CompletionTokens)
				}
			case <-time.After(2 * time.Second):
				t.Error("the billed reasoning tokens were not recorded")
			}
		})
	}
}

// TestStream_TruncatedAnswerAtTokenCapStillDone: an answer cut at the cap
// AFTER some text keeps it — the truncated answer finishes as before.
func TestStream_TruncatedAnswerAtTokenCapStillDone(t *testing.T) {
	for _, path := range chatPaths {
		t.Run(path.name, func(t *testing.T) {
			p := &scriptedChunkProvider{FakeProvider: &testutil.FakeProvider{}, tools: path.native, turns: [][]ai.Chunk{{
				{Text: "The flow has three issues"},
				{Done: true, FinishReason: "length", TokensIn: 5, TokensOut: 7},
			}}}
			types, _, _ := runScriptedStream(t, p, path.useTools)
			if indexOf(types, "done") < 0 || indexOf(types, "error") >= 0 {
				t.Errorf("events = %v, want the truncated answer to finish with done", types)
			}
		})
	}
}

// TestStream_EmptyStopIsNotOutputLimit: only a stop AT the token cap is the
// exhausted-budget failure; other finish reasons keep their behaviour.
func TestStream_EmptyStopIsNotOutputLimit(t *testing.T) {
	p := &scriptedChunkProvider{FakeProvider: &testutil.FakeProvider{}, turns: [][]ai.Chunk{{
		{Done: true, FinishReason: "stop", TokensIn: 5, TokensOut: 1},
	}}}
	types, _, _ := runScriptedStream(t, p, false)
	if indexOf(types, "done") < 0 || indexOf(types, "error") >= 0 {
		t.Errorf("events = %v", types)
	}
}

func TestIsOutputLimitFinish(t *testing.T) {
	for reason, want := range map[string]bool{
		"length":     true, // OpenAI-compatible chat completions, Responses incomplete
		"max_tokens": true, // Claude
		"MAX_TOKENS": true, // Gemini
		// Claude 4.5+: the context window filled up during generation.
		"model_context_window_exceeded": true,
		"stop":                          false,
		"tool_calls":                    false,
		"":                              false,
	} {
		if got := isOutputLimitFinish(reason); got != want {
			t.Errorf("isOutputLimitFinish(%q) = %v, want %v", reason, got, want)
		}
	}
	if emptyOutputLimitTurn(false, 1, "length") {
		t.Error("a turn with tool calls is not an empty answer")
	}
	if emptyOutputLimitTurn(true, 0, "length") {
		t.Error("a turn with text is not an empty answer")
	}
}

// TestStreamCtlFailure_OutputLimitCopy pins the copy the client shows (it
// renders unknown codes' messages as-is).
func TestStreamCtlFailure_OutputLimitCopy(t *testing.T) {
	if ChatErrOutputLimit != "PROVIDER_OUTPUT_LIMIT" {
		t.Errorf("code = %q", ChatErrOutputLimit)
	}
	for _, want := range []string{"Max tokens", "reasoning models", "Settings → AI Behavior"} {
		if !strings.Contains(outputLimitMsg, want) {
			t.Errorf("message %q lacks %q", outputLimitMsg, want)
		}
	}
}
