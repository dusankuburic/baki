package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// Documentation-conformance fixes F1, service side: the tool loop carries
// each turn's provider record (ai.Message.ProviderItems) append-only, strips
// every record on the first history rewrite, never runs a tool call cut off
// at an output limit, and raises reasoning models' max tokens to their floor.

// itemsTurn is one scripted Stream call: its chunks, or an error.
type itemsTurn struct {
	chunks []ai.Chunk
	err    error
}

// itemsProvider is a native-tools provider with a configurable ID that
// records every request it receives and answers from a script.
type itemsProvider struct {
	testutil.FakeProvider
	id      string
	limit   int            // ContextLimit (0 = the fake's default)
	weights map[string]int // EstimateTokens by exact text; nil = len/4
	models  []ai.ModelInfo
	turns   []itemsTurn
}

func (p *itemsProvider) ID() string          { return p.id }
func (p *itemsProvider) SupportsTools() bool { return true }
func (p *itemsProvider) ContextLimit() int {
	if p.limit > 0 {
		return p.limit
	}
	return p.FakeProvider.ContextLimit()
}
func (p *itemsProvider) EstimateTokens(s string) int {
	if p.weights == nil {
		return len(s) / 4
	}
	return p.weights[s]
}
func (p *itemsProvider) Models(context.Context) ([]ai.ModelInfo, error) { return p.models, nil }

// Stream records req with the shared fake's recorder and answers from the script.
func (p *itemsProvider) Stream(_ context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	i := p.Record(req)
	if i >= len(p.turns) {
		onChunk(ai.Chunk{Text: "done"})
		onChunk(ai.Chunk{Done: true, FinishReason: "end_turn"})
		return nil
	}
	if p.turns[i].err != nil {
		return p.turns[i].err
	}
	for _, c := range p.turns[i].chunks {
		onChunk(c)
	}
	return nil
}

func (p *itemsProvider) requests() []ai.Request { return p.Requests() }

// toolTurn is a scripted turn that calls one tool and carries a record.
func toolTurn(id, name, input, items string) itemsTurn {
	var rec json.RawMessage
	if items != "" {
		rec = json.RawMessage(items)
	}
	return itemsTurn{chunks: []ai.Chunk{
		{Text: "Checking."},
		{Done: true, FinishReason: "tool_use", TokensIn: 5, TokensOut: 5, ProviderItems: rec,
			ToolCalls: []ai.ToolCall{{ID: id, Name: name, Input: json.RawMessage(input)}}},
	}}
}

// runItemsLoop drives runToolLoop against p and returns the stream control
// and the emitted event types and payloads.
func runItemsLoop(t *testing.T, p *itemsProvider, msgs []ai.Message) (*streamCtl, []string, []map[string]interface{}) {
	t.Helper()
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{cancel: func() {}, started: make(chan struct{})}
	close(ctl.started)
	var types []string
	var payloads []map[string]interface{}
	emit := func(typ string, data map[string]interface{}) {
		types = append(types, typ)
		payloads = append(payloads, data)
	}
	svc.runToolLoop(context.Background(), p, ai.Request{Model: "m", Messages: msgs}, "user-1",
		&ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })
	return ctl, types, payloads
}

// itemsOf lists (index, from, items) for every message carrying a record.
func itemsOf(msgs []ai.Message) []string {
	var out []string
	for i, m := range msgs {
		if len(m.ProviderItems) > 0 || m.ProviderItemsFrom != "" {
			out = append(out, strings.Join([]string{strconv.Itoa(i), m.ProviderItemsFrom, string(m.ProviderItems)}, "|"))
		}
	}
	return out
}

const (
	recordA = `[{"type":"thinking","thinking":"","signature":"sigA"},{"type":"tool_use","id":"t1","name":"search_flow","input":{"query":"xav"}}]`
	recordB = `[{"type":"thinking","thinking":"","signature":"sigB"},{"type":"tool_use","id":"t2","name":"get_block","input":{"blockId":"b1"}}]`
)

// The loop appends each tool-calling turn with its provider record, tagged
// with the provider's ID, and replays the history append-only: request 3
// carries both records unchanged, in order.
func TestRunToolLoop_CarriesProviderItemsAppendOnly(t *testing.T) {
	p := &itemsProvider{id: "claude", turns: []itemsTurn{
		toolTurn("t1", "search_flow", `{"query":"xav"}`, recordA),
		toolTurn("t2", "get_block", `{"blockId":"b1"}`, recordB),
		{chunks: []ai.Chunk{{Text: "Final"}, {Done: true, FinishReason: "end_turn"}}},
	}}
	ctl, types, _ := runItemsLoop(t, p, []ai.Message{{Role: "user", Content: "analyse"}})
	if indexOf(types, "done") < 0 || ctl.errMsg != "" {
		t.Fatalf("events %v, failure %q", types, ctl.errMsg)
	}
	reqs := p.requests()
	if len(reqs) != 3 {
		t.Fatalf("got %d requests", len(reqs))
	}
	if got := itemsOf(reqs[0].Messages); got != nil {
		t.Errorf("request 1 carries records: %v", got)
	}
	if got, want := itemsOf(reqs[1].Messages), []string{"1|claude|" + recordA}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("request 2 records = %v, want %v", got, want)
	}
	want := []string{"1|claude|" + recordA, "3|claude|" + recordB}
	if got := itemsOf(reqs[2].Messages); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("request 3 records = %v, want %v", got, want)
	}
	if strings.Contains(ctl.buffer.String(), "sigA") {
		t.Error("a provider record reached the client-visible buffer")
	}
}

// Truncation mid-loop drops earlier messages: from that request on, no
// message carries a record — the surviving turns' records are stripped and
// later turns' records are never added.
func TestRunToolLoop_TruncationStripsEveryProviderItem(t *testing.T) {
	const bigInput = `{"query":"big"}`
	n := len(ai.ToolDefinitions())
	// Token weights (everything else is free): the old history weighs 1000,
	// the second call's input 800. The window holds the prompt until that
	// input joins it; dropping the old history makes it fit again.
	p := &itemsProvider{
		id:      "claude",
		limit:   contextReserve + 8*n + 1300,
		weights: map[string]int{"OLD-HISTORY": 1000, bigInput: 800},
		turns: []itemsTurn{
			toolTurn("t1", "search_flow", `{"query":"x"}`, recordA),
			toolTurn("t2", "search_flow", bigInput, `[{"type":"tool_use","id":"t2","name":"search_flow","input":{"query":"big"}}]`),
			toolTurn("t3", "search_flow", `{"query":"y"}`, `[{"type":"tool_use","id":"t3","name":"search_flow","input":{"query":"y"}}]`),
			{chunks: []ai.Chunk{{Text: "Final"}, {Done: true, FinishReason: "end_turn"}}},
		},
	}
	ctl, types, _ := runItemsLoop(t, p, []ai.Message{
		{Role: "user", Content: "OLD-HISTORY"},
		{Role: "assistant", Content: "earlier answer"},
		{Role: "user", Content: "current question"},
	})
	if indexOf(types, "done") < 0 || ctl.errMsg != "" {
		t.Fatalf("events %v, failure %q", types, ctl.errMsg)
	}
	reqs := p.requests()
	if len(reqs) != 4 {
		t.Fatalf("got %d requests", len(reqs))
	}
	if len(reqs[1].Messages) != 5 || len(itemsOf(reqs[1].Messages)) != 1 {
		t.Fatalf("request 2 (before truncation) = %d messages, records %v", len(reqs[1].Messages), itemsOf(reqs[1].Messages))
	}
	if reqs[2].Messages[0].Content != "current question" {
		t.Fatalf("request 3 must have dropped the old history, starts with %q", reqs[2].Messages[0].Content)
	}
	for i, r := range reqs[2:] {
		if got := itemsOf(r.Messages); got != nil {
			t.Errorf("request %d after truncation carries records %v", i+3, got)
		}
	}
}

// A record from another provider means the provider changed under the
// history: nothing is replayed, and the loop records nothing from then on.
func TestRunToolLoop_ProviderSwitchNeverReplays(t *testing.T) {
	p := &itemsProvider{id: "claude", turns: []itemsTurn{
		toolTurn("t2", "search_flow", `{"query":"x"}`, recordB),
		{chunks: []ai.Chunk{{Text: "Final"}, {Done: true, FinishReason: "end_turn"}}},
	}}
	_, types, _ := runItemsLoop(t, p, []ai.Message{
		{Role: "user", Content: "analyse"},
		{Role: "assistant", Content: "Checking.", ToolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: json.RawMessage(`{"query":"xav"}`)}},
			ProviderItems: json.RawMessage(recordA), ProviderItemsFrom: "azure-foundry"},
		{Role: "tool", ToolCallID: "t1", Content: "1 match"},
		{Role: "user", Content: "and now?"},
	})
	if indexOf(types, "done") < 0 {
		t.Fatalf("events %v", types)
	}
	for i, r := range p.requests() {
		if got := itemsOf(r.Messages); got != nil {
			t.Errorf("request %d carries records %v after a provider switch", i+1, got)
		}
	}
}

// The tools-unsupported fallback changes the request (tools dropped): the
// retried request carries no record.
func TestRunToolLoop_ToolsFallbackStripsProviderItems(t *testing.T) {
	p := &itemsProvider{id: "azure-foundry", turns: []itemsTurn{
		toolTurn("t1", "search_flow", `{"query":"xav"}`, recordA),
		{err: ai.ErrToolsUnsupported},
		{chunks: []ai.Chunk{{Text: "Final"}, {Done: true, FinishReason: "end_turn"}}},
	}}
	_, types, _ := runItemsLoop(t, p, []ai.Message{{Role: "user", Content: "analyse"}})
	reqs := p.requests()
	if indexOf(types, "done") < 0 || len(reqs) != 3 {
		t.Fatalf("events %v, %d requests", types, len(reqs))
	}
	if got := itemsOf(reqs[1].Messages); len(got) != 1 {
		t.Errorf("request 2 must carry the first turn's record, got %v", got)
	}
	if len(reqs[2].Tools) != 0 {
		t.Errorf("the fallback request must drop the tools")
	}
	if got := itemsOf(reqs[2].Messages); got != nil {
		t.Errorf("the fallback request carries records %v", got)
	}
}

// A tool call cut off at an OUTPUT LIMIT is never executed: the turn fails
// with PROVIDER_OUTPUT_LIMIT and no tool runs — whatever the provider. (A
// non-limit finish with malformed argument JSON is the case below instead.)
func TestRunToolLoop_TruncatedToolCallIsNotExecuted(t *testing.T) {
	for _, tc := range []struct{ finish, input string }{
		{"max_tokens", `{"query":"xav"}`},
		{"length", `{"query":"xav"}`},
		{"MAX_TOKENS", `{"query":"xav"}`},
		{"model_context_window_exceeded", `{"query":"xav"}`},
	} {
		p := &itemsProvider{id: "claude", turns: []itemsTurn{{chunks: []ai.Chunk{
			{Text: "Let me look"},
			{Done: true, FinishReason: tc.finish, ToolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: json.RawMessage(tc.input)}}},
		}}}}
		ctl, types, payloads := runItemsLoop(t, p, []ai.Message{{Role: "user", Content: "analyse"}})
		name := tc.finish + " " + tc.input
		if n := len(p.requests()); n != 1 {
			t.Errorf("%s: %d provider calls, want 1 (no tool result sent back)", name, n)
		}
		if indexOf(types, "tool") >= 0 || indexOf(types, "tool_result") >= 0 || indexOf(types, "done") >= 0 {
			t.Errorf("%s: events %v — the cut-off call must not run", name, types)
		}
		at := indexOf(types, "error")
		if at < 0 || payloads[at]["code"] != ChatErrOutputLimit || ctl.errCode != ChatErrOutputLimit {
			t.Errorf("%s: events %v, ctl code %q — want a %s error", name, types, ctl.errCode, ChatErrOutputLimit)
		}
	}
	// Empty arguments are the providers' "no arguments" and still run.
	p := &itemsProvider{id: "claude", turns: []itemsTurn{toolTurn("t1", "list_findings", ``, "")}}
	_, types, _ := runItemsLoop(t, p, []ai.Message{{Role: "user", Content: "analyse"}})
	if indexOf(types, "tool") < 0 || indexOf(types, "done") < 0 {
		t.Errorf("a call with empty arguments must run, events %v", types)
	}
}

// Malformed argument JSON with a NON-limit finish must not abort the
// turn with the raise-Max-tokens copy. The call is announced, answered with a
// retryable "not valid JSON" tool result (ok=false), the loop sends that
// result back (second provider request whose last message is the error tool
// result), the replayed assistant record marshals (invalid input replaced
// with {}), and the model's second answer finishes the turn with done.
func TestRunToolLoop_MalformedToolArgumentsGetRetryableResult(t *testing.T) {
	for _, tc := range []struct{ name, finish, input string }{
		{"claude tool_use", "tool_use", `{"query":"xa`},
		{"open tool_calls", "tool_calls", `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &itemsProvider{id: "claude", turns: []itemsTurn{{chunks: []ai.Chunk{
				{Text: "Let me look"},
				{Done: true, FinishReason: tc.finish, ToolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: json.RawMessage(tc.input)}}},
			}}}}
			ctl, types, payloads := runItemsLoop(t, p, []ai.Message{{Role: "user", Content: "analyse"}})

			if indexOf(types, "error") >= 0 || ctl.errCode != "" {
				t.Fatalf("events %v, ctl code %q — a malformed-arguments call must not fail the turn", types, ctl.errCode)
			}
			if indexOf(types, "done") < 0 {
				t.Fatalf("events %v — the model's retry must finish the turn", types)
			}
			// The call is announced and its result reports ok=false, naming
			// the problem so the model can re-issue the call.
			toolAt := indexOf(types, "tool")
			resAt := indexOf(types, "tool_result")
			if toolAt < 0 || resAt < 0 || resAt < toolAt {
				t.Fatalf("events %v — want the call announced then its result", types)
			}
			if payloads[resAt]["ok"] != false || !strings.Contains(fmt.Sprint(payloads[resAt]["summary"]), "not valid JSON") {
				t.Errorf("tool_result payload = %v, want ok=false naming the invalid JSON", payloads[resAt])
			}
			// Two provider requests; the second's last message is the error
			// tool result the model reads to retry.
			reqs := p.requests()
			if len(reqs) != 2 {
				t.Fatalf("%d provider requests, want 2", len(reqs))
			}
			second := reqs[1].Messages
			if last := second[len(second)-1]; last.Role != "tool" || !strings.HasPrefix(last.Content, "error:") {
				t.Errorf("second request's last message = %s/%q, want the error tool result", last.Role, last.Content)
			}
			// The recorded assistant turn marshals: its invalid input became
			// an empty object while the executed call saw the original bytes.
			var assistant *ai.Message
			for i := range second {
				if second[i].Role == "assistant" && len(second[i].ToolCalls) > 0 {
					assistant = &second[i]
				}
			}
			if assistant == nil {
				t.Fatal("no assistant tool-call turn in the second request")
			}
			if got := string(assistant.ToolCalls[0].Input); got != "{}" {
				t.Errorf("recorded call input = %q, want {} (the replay must marshal)", got)
			}
		})
	}
	// A valid call with the same non-limit finish still executes normally.
	p := &itemsProvider{id: "claude", turns: []itemsTurn{toolTurn("t1", "search_flow", `{"query":"xav"}`, "")}}
	_, types, payloads := runItemsLoop(t, p, []ai.Message{{Role: "user", Content: "analyse"}})
	if resAt := indexOf(types, "tool_result"); resAt < 0 || payloads[resAt]["ok"] != true {
		t.Errorf("events %v — a valid call must execute, not error", types)
	}
}

// The reasoning floor: a reasoning model's turn gets at least 16384 max
// tokens (known by its id or the model its listing entry serves), re-capped
// by the model's output ceiling and the context-window backstop; other
// models keep the caller's value.
func TestTurnGenerationParams_ReasoningFloor(t *testing.T) {
	p := &itemsProvider{id: "azure-foundry", limit: 1 << 20, models: []ai.ModelInfo{
		{ID: "gpt-5"},
		{ID: "gpt-4o", MaxOutputTokens: 16384},
		{ID: "o4-mini-small", MaxOutputTokens: 8192},
		{ID: "prod-reasoner", ServedModel: "gpt-5-mini"},
		{ID: "claude:opus", ServedModel: "claude-opus-5-5", MaxOutputTokens: 128000},
		{ID: "grok-4", MaxOutputTokens: 8192},
		{ID: "model-router", ServedModel: "gpt-4.1-mini"},
		{ID: "claude:prod-opus"},
		{ID: "claude:claude-haiku-4-5", ServedModel: "claude-haiku-4-5", MaxOutputTokens: 64000},
	}}
	for _, tc := range []struct {
		model string
		in    int
		want  int
	}{
		{"gpt-5", 4096, 16384},
		{"gpt-5", 32000, 32000}, // above the floor: unchanged
		{"gpt-5", 0, 16384},     // unset: the providers' defaults are below the floor
		{"gpt-4o", 4096, 4096},  // not a reasoning model
		{"o4-mini-small", 4096, 8192},
		{"prod-reasoner", 4096, 16384}, // served model reveals the reasoner
		{"claude:opus", 4096, 16384},
		// grok-4's documented cap (8192) re-clamps the floor:
		// the catalogue limits, not the floor, bound the request.
		{"grok-4", 4096, 8192},
		// A model router always gets the floor: the last
		// routed non-reasoner must not suppress it for the next call.
		{"model-router", 4096, 16384},
		// A Claude deployment whose model nobody knows yet is floored
		// while a named non-thinking Claude model is not.
		{"claude:prod-opus", 4096, 16384},
		{"claude:claude-haiku-4-5", 4096, 4096},
	} {
		_, got := turnGenerationParams(context.Background(), p, models.ChatRequest{Model: tc.model, MaxTokens: tc.in}, turnModel{resolved: tc.model})
		if got != tc.want {
			t.Errorf("%s with %d: max tokens %d, want %d", tc.model, tc.in, got, tc.want)
		}
	}
	// The context-window backstop still caps the floor.
	small := &itemsProvider{id: "x", limit: contextReserve + 10000, models: []ai.ModelInfo{{ID: "gpt-5"}}}
	if _, got := turnGenerationParams(context.Background(), small, models.ChatRequest{Model: "gpt-5", MaxTokens: 4096}, turnModel{resolved: "gpt-5"}); got != 10000 {
		t.Errorf("backstop: max tokens %d, want 10000", got)
	}
	// The backstop is the MODEL's window (the listing entry), not the
	// provider-wide placeholder — a 1M-window Claude deployment's 128000
	// output cap must not be clamped to 124000 by the Azure 128000 default.
	big := &itemsProvider{id: "azure-foundry", limit: 128000, models: []ai.ModelInfo{
		{ID: "claude:opus", ServedModel: "claude-opus-5-5", ContextLimit: 1_000_000, MaxOutputTokens: 128000},
	}}
	if _, got := turnGenerationParams(context.Background(), big, models.ChatRequest{Model: "claude:opus", MaxTokens: 128000}, turnModel{resolved: "claude:opus"}); got != 128000 {
		t.Errorf("model-window backstop: max tokens %d, want 128000", got)
	}
}

// End to end through the factory: the provider receives the raised max
// tokens and the caller's explicit temperature 0 marked as set.
func TestStreamChatMessage_SendsReasoningFloorAndTemperatureSet(t *testing.T) {
	p := &itemsProvider{id: "floor-fake", limit: 1 << 20, models: []ai.ModelInfo{{ID: "gpt-5"}, {ID: "gpt-4o"}}}
	const id = "floor-fake"
	ai.RegisterProvider(id, func(string) ai.Provider { return p })
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil,
		func(context.Context, *interfaces.UsageMetric) error { return nil }, nil)
	svc := &ChatService{
		notifier:      &testutil.CountingNotifier{},
		configDir:     t.TempDir(),
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
		settings:      nilSettings{},
		chatCtxCache:  newChatContextCache(),
	}
	run := func(req models.ChatRequest) ai.Request {
		t.Helper()
		before := len(p.requests())
		req.Provider = id
		req.UserMessage = "analyse this flow"
		streamID, err := svc.StreamChatMessage(context.Background(), "test", &models.FlowDocument{ID: "f1", Name: "f1"}, nil, req)
		if err != nil {
			t.Fatalf("StreamChatMessage: %v", err)
		}
		svc.BeginStream(context.Background(), streamID)
		deadline := time.Now().Add(5 * time.Second)
		for len(p.requests()) == before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		reqs := p.requests()
		if len(reqs) == before {
			t.Fatal("the provider was not called")
		}
		// Wait for the worker's cleanup to finish (it stores the answer in
		// the conversation directory after leaving the active set).
		for time.Now().Before(deadline) {
			if v, ok := svc.finishedStreams.Load(streamID); ok {
				select {
				case <-v.(*streamCtl).settled:
					return reqs[before]
				case <-time.After(time.Until(deadline)):
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("the stream did not settle within 5s")
		return ai.Request{}
	}

	got := run(models.ChatRequest{Model: "gpt-5", MaxTokens: 4096, Temperature: 0, TemperatureSet: true})
	if got.MaxTokens != 16384 {
		t.Errorf("gpt-5 was sent max tokens %d, want the 16384 floor", got.MaxTokens)
	}
	if !got.TemperatureSet || got.Temperature != 0 {
		t.Errorf("explicit temperature 0 = (%v, set %v), want (0, set)", got.Temperature, got.TemperatureSet)
	}
	got = run(models.ChatRequest{Model: "gpt-4o", MaxTokens: 4096})
	if got.MaxTokens != 4096 || got.TemperatureSet {
		t.Errorf("gpt-4o was sent max tokens %d (temperature set %v), want 4096 and unset", got.MaxTokens, got.TemperatureSet)
	}
	// The validated per-provider reasoning-effort choice reaches the
	// provider request; "" (the default) is omitted.
	got = run(models.ChatRequest{Model: "gpt-5", MaxTokens: 4096, ReasoningEffort: "high"})
	if got.ReasoningEffort != "high" {
		t.Errorf("reasoning effort = %q, want high", got.ReasoningEffort)
	}
	if got = run(models.ChatRequest{Model: "gpt-5", MaxTokens: 4096}); got.ReasoningEffort != "" {
		t.Errorf("unset reasoning effort = %q, want the provider default", got.ReasoningEffort)
	}
	// An out-of-vocabulary value is refused before any provider call.
	if _, err := svc.StreamChatMessage(context.Background(), "test", &models.FlowDocument{ID: "f1"}, nil,
		models.ChatRequest{Provider: id, Model: "gpt-5", UserMessage: "q", ReasoningEffort: "xhigh"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("reasoningEffort xhigh: err = %v, want ErrInvalidInput", err)
	}
}

// A provider's output-limit error (a non-streaming Claude call cut at
// max_tokens with nothing to show) maps to PROVIDER_OUTPUT_LIMIT.
func TestClassifyFailure_OutputLimitError(t *testing.T) {
	ctl := &streamCtl{}
	err := fmt.Errorf("claude: %w (stop_reason max_tokens)", ai.ErrOutputLimit)
	if code, msg := ctl.classifyFailure(context.Background(), err); code != ChatErrOutputLimit || msg != outputLimitMsg {
		t.Errorf("classifyFailure = (%q, %q), want (%q, the output-limit copy)", code, msg, ChatErrOutputLimit)
	}
}
