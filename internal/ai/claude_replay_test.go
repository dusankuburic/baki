package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Documentation-conformance fixes F1 for the Claude Messages client (direct
// and on Microsoft Foundry): thinking preserved across tool steps, output
// limits, sampling, tool errors, request ids, the Foundry host.

// claudeToolTurnSSE is a thinking model's tool-use turn as the API streams it
// with display "omitted": an empty thinking block whose signature arrives as
// a signature_delta, a text preamble, then a tool_use with streamed input.
func claudeToolTurnSSE(signature string) string {
	return sseEvent("message_start", `{"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":30}}}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"`+signature+`"}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me "}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"search."}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"search_flow","input":{}}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"xav\"}"}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":2}`) +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}`) +
		sseEvent("message_stop", `{"type":"message_stop"}`)
}

const claudeFinalTurnSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5-5\",\"usage\":{\"input_tokens\":50}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"found it\"}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// claudeBodies records the decoded request bodies a stub server received.
type claudeBodies struct {
	mu     sync.Mutex
	bodies []map[string]any
}

// add records r's body and returns how many requests were seen so far.
func (b *claudeBodies) add(r *http.Request) int {
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bodies = append(b.bodies, body)
	return len(b.bodies)
}

func (b *claudeBodies) all() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]map[string]any(nil), b.bodies...)
}

// jsonEqual compares two JSON documents semantically (key order aside).
func jsonEqual(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v (%s)", err, want)
	}
	return reflect.DeepEqual(g, w)
}

// streamTurn runs one Stream call and returns its Done chunk.
func streamTurn(t *testing.T, p *ClaudeProvider, req Request) Chunk {
	t.Helper()
	var done Chunk
	if err := p.Stream(context.Background(), req, func(c Chunk) {
		if c.Done {
			done = c
		}
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return done
}

// A two-iteration tool loop on a thinking model: the first turn's blocks —
// the empty thinking block with its signature, the text, the tool_use — are
// recorded on the Done chunk and sent back in the second request verbatim and
// in order, exactly as the chat service's loop appends the turn.
func TestClaudeToolLoop_ReplaysThinkingVerbatim(t *testing.T) {
	rec := &claudeBodies{}
	p, _ := newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) {
		n := rec.add(r)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = io.WriteString(w, claudeToolTurnSSE("sig-A=="))
			return
		}
		_, _ = io.WriteString(w, claudeFinalTurnSSE)
	})
	tools := []ToolDefinition{{Name: "search_flow", Description: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	req := Request{Model: "claude-opus-5-5", Tools: tools, Messages: []Message{{Role: "user", Content: "find xav"}}}

	first := streamTurn(t, p, req)
	wantBlocks := `[{"type":"thinking","thinking":"","signature":"sig-A=="},` +
		`{"type":"text","text":"Let me search."},` +
		`{"type":"tool_use","id":"toolu_1","name":"search_flow","input":{"query":"xav"}}]`
	if !jsonEqual(t, string(first.ProviderItems), wantBlocks) {
		t.Fatalf("Done.ProviderItems = %s, want %s", first.ProviderItems, wantBlocks)
	}
	if len(first.ToolCalls) != 1 || string(first.ToolCalls[0].Input) != `{"query":"xav"}` {
		t.Fatalf("tool calls = %+v", first.ToolCalls)
	}

	req.Messages = append(req.Messages,
		Message{Role: "assistant", Content: "Let me search.", ToolCalls: first.ToolCalls, ProviderItems: first.ProviderItems, ProviderItemsFrom: p.ID()},
		Message{Role: "tool", ToolCallID: "toolu_1", Content: `{"hits":1}`})
	if second := streamTurn(t, p, req); second.FinishReason != "end_turn" {
		t.Fatalf("second turn = %+v", second)
	}

	bodies := rec.all()
	msgs, _ := bodies[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second request messages = %v", bodies[1]["messages"])
	}
	assistant, _ := msgs[1].(map[string]any)
	got, _ := json.Marshal(assistant["content"])
	if assistant["role"] != "assistant" || !jsonEqual(t, string(got), wantBlocks) {
		t.Errorf("replayed assistant turn = %s, want the recorded blocks %s", got, wantBlocks)
	}
	// Opus 5.5 with tools: no temperature, and adaptive thinking is asked
	// for explicitly on every loop iteration — omitted IS adaptive on 5.x,
	// and explicit is what Opus 4.7/4.8 need.
	for _, b := range bodies {
		if _, ok := b["temperature"]; ok {
			t.Errorf("temperature sent to a model that rejects sampling: %v", b["temperature"])
		}
		think, _ := b["thinking"].(map[string]any)
		if think["type"] != "adaptive" {
			t.Errorf("thinking must be adaptive on every loop request, got %v", b["thinking"])
		}
		// The top-level automatic-caching marker rides every loop request,
		// so the history prefix and tool_results behind the system block are
		// cache reads from iteration 2 on.
		if top, _ := b["cache_control"].(map[string]any); top["type"] != "ephemeral" {
			t.Errorf("top-level cache_control must be ephemeral, got %v", b["cache_control"])
		}
	}
	// The marker never lands on the replayed assistant blocks themselves.
	if msgs, _ := bodies[1]["messages"].([]any); len(msgs) == 3 {
		if assistant, _ := msgs[1].(map[string]any); assistant != nil {
			if blocks, _ := assistant["content"].([]any); len(blocks) > 0 {
				if b0, _ := blocks[0].(map[string]any); b0 != nil {
					if _, has := b0["cache_control"]; has {
						t.Error("replayed assistant blocks must carry no per-block cache_control marker")
					}
				}
			}
		}
	} else {
		t.Fatalf("second request messages = %v", bodies[1]["messages"])
	}
}

// assistantContent is the wire content toClaudeMessages sends for msgs[i].
func assistantContent(t *testing.T, msgs []Message, owner string, i int) string {
	t.Helper()
	raw, err := json.Marshal(toClaudeMessages(msgs, owner)[i].Content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// A record is replayed only by the provider that wrote it and only for the
// turn it describes; anything else rebuilds the turn from its text and tool
// calls — a provider switch never replays a foreign record.
func TestToClaudeMessages_ReplayOnlyOwnMatchingRecord(t *testing.T) {
	record := json.RawMessage(`[{"type":"thinking","thinking":"","signature":"s1"},{"type":"redacted_thinking","data":"enc"},` +
		`{"type":"text","text":"Looking"},{"type":"tool_use","id":"toolu_1","name":"search_flow","input":{"q":"x"}}]`)
	calls := []ToolCall{{ID: "toolu_1", Name: "search_flow", Input: json.RawMessage(`{"q":"x"}`)}}
	turn := func(from string, items json.RawMessage, calls []ToolCall) []Message {
		return []Message{
			{Role: "user", Content: "q"},
			{Role: "assistant", Content: "Looking", ToolCalls: calls, ProviderItems: items, ProviderItemsFrom: from},
			{Role: "tool", ToolCallID: "toolu_1", Content: "r"},
		}
	}
	rebuilt := `[{"type":"text","text":"Looking"},{"type":"tool_use","id":"toolu_1","name":"search_flow","input":{"q":"x"}}]`

	if got := assistantContent(t, turn("claude", record, calls), "claude", 1); !jsonEqual(t, got, string(record)) {
		t.Errorf("own record: %s, want it verbatim", got)
	}
	if got := assistantContent(t, turn(azureFoundryProviderID, record, calls), azureFoundryProviderID, 1); !jsonEqual(t, got, string(record)) {
		t.Errorf("Foundry-tagged record on the Foundry route: %s, want it verbatim", got)
	}
	for name, msgs := range map[string][]Message{
		"another provider's record":  turn(azureFoundryProviderID, record, calls),
		"untagged record":            turn("", record, calls),
		"record of other tool calls": turn("claude", record, []ToolCall{{ID: "toolu_9", Name: "search_flow", Input: json.RawMessage(`{"q":"x"}`)}}),
		"Responses items":            turn("claude", json.RawMessage(`[{"type":"reasoning","encrypted_content":"x"},{"type":"function_call","call_id":"toolu_1"}]`), calls),
		"not an array":               turn("claude", json.RawMessage(`{"type":"text"}`), calls),
	} {
		got := assistantContent(t, msgs, "claude", 1)
		want := rebuilt
		if name == "record of other tool calls" {
			want = `[{"type":"text","text":"Looking"},{"type":"tool_use","id":"toolu_9","name":"search_flow","input":{"q":"x"}}]`
		}
		if !jsonEqual(t, got, want) {
			t.Errorf("%s: %s, want the rebuilt turn %s", name, got, want)
		}
	}
}

// The stream record keeps every block in index order: a summarized thinking
// block with its text and signature, a redacted_thinking block's data, the
// text and the tool_use — and a multi-MiB signature fits the SSE line cap.
func TestParseClaudeSSE_RecordsEveryBlockInOrder(t *testing.T) {
	bigSig := strings.Repeat("A", 3<<20) // > the old 1 MiB line cap
	sse := sseEvent("message_start", `{"type":"message_start","message":{"model":"claude-fable-5-1","usage":{"input_tokens":5}}}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one; "}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step two"}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"`+bigSig+`"}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"ENCRYPTED"}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"ok"}}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_7","name":"get_block","input":{}}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":3}`) +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`)

	var done Chunk
	var text, reasoning string
	if err := parseClaudeSSE(strings.NewReader(sse), func(c Chunk) {
		text += c.Text
		reasoning += c.Reasoning
		if c.Done {
			done = c
		}
	}); err != nil {
		t.Fatalf("parseClaudeSSE: %v", err)
	}
	want := `[{"type":"thinking","thinking":"step one; step two","signature":"` + bigSig + `"},` +
		`{"type":"redacted_thinking","data":"ENCRYPTED"},{"type":"text","text":"ok"},` +
		`{"type":"tool_use","id":"toolu_7","name":"get_block","input":{}}]`
	if !jsonEqual(t, string(done.ProviderItems), want) {
		t.Errorf("record = %.300s…", done.ProviderItems)
	}
	if text != "ok" || reasoning != "" {
		t.Errorf("thinking must never become text: text %q reasoning %q", text, reasoning)
	}
	if len(done.ToolCalls) != 1 || string(done.ToolCalls[0].Input) != "{}" {
		t.Errorf("tool calls = %+v", done.ToolCalls)
	}
}

// A tool_use cut off at max_tokens has input that is not JSON: the call is
// still reported (the chat service refuses to run it) but no record is made.
func TestParseClaudeSSE_TruncatedToolUseHasNoRecord(t *testing.T) {
	sse := sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"search_flow","input":{}}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"xa"}}`) +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":9}}`)
	var done Chunk
	if err := parseClaudeSSE(strings.NewReader(sse), func(c Chunk) {
		if c.Done {
			done = c
		}
	}); err != nil {
		t.Fatalf("parseClaudeSSE: %v", err)
	}
	if done.FinishReason != "max_tokens" || len(done.ToolCalls) != 1 || json.Valid(done.ToolCalls[0].Input) {
		t.Errorf("done = %+v, want the cut-off call reported with its partial input", done)
	}
	if done.ProviderItems != nil {
		t.Errorf("a record with an unfinished tool_use must not be made, got %s", done.ProviderItems)
	}
}

// A mid-stream timeout_error (the API's 504 after the 200) is transient:
// ErrProviderDown, retried when it arrives before any content.
func TestClassifyClaudeStreamError_TimeoutIsProviderDown(t *testing.T) {
	err := classifyClaudeStreamError("timeout_error", "Request timed out")
	if !errors.Is(err, ErrProviderDown) || !isRetryable(err) {
		t.Errorf("timeout_error = %v, want retryable ErrProviderDown", err)
	}
}

// The non-streaming path records the content blocks exactly as received.
func TestClaudeChat_RecordsContentBlocks(t *testing.T) {
	content := `[{"type":"thinking","thinking":"","signature":"sig-9"},{"type":"text","text":"Looking"},` +
		`{"type":"tool_use","id":"toolu_1","name":"search_flow","input":{"q":"x"}}]`
	p, _ := newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"claude-opus-5","stop_reason":"tool_use","content":`+content+`,"usage":{"input_tokens":3,"output_tokens":4}}`)
	})
	resp, err := p.Chat(context.Background(), Request{Model: "claude-opus-5", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !jsonEqual(t, string(resp.ProviderItems), content) || resp.Content != "Looking" || len(resp.ToolCalls) != 1 {
		t.Errorf("response = %+v (items %s)", resp, resp.ProviderItems)
	}
}

// A non-streaming response cut at max_tokens or at the context window with
// no text and no tool_use (a thinking model spent the budget) is the output
// limit error, not an empty success; with text it is the truncated answer.
func TestClaudeChat_EmptyOutputLimitStopIsError(t *testing.T) {
	for _, tc := range []struct {
		stop, content string
		wantErr       bool
	}{
		{"max_tokens", `[{"type":"thinking","thinking":"","signature":"s"}]`, true},
		{"model_context_window_exceeded", `[{"type":"thinking","thinking":"","signature":"s"}]`, true},
		{"max_tokens", `[]`, true},
		{"max_tokens", `[{"type":"text","text":"partial"}]`, false},
		{"end_turn", `[{"type":"thinking","thinking":"","signature":"s"}]`, false},
	} {
		p, _ := newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"model":"claude-opus-5-5","stop_reason":"`+tc.stop+`","content":`+tc.content+`,"usage":{"input_tokens":3,"output_tokens":16}}`)
		})
		_, err := p.Chat(context.Background(), Request{Model: "claude-opus-5-5", MaxTokens: 16, Messages: []Message{{Role: "user", Content: "x"}}})
		if got := errors.Is(err, ErrOutputLimit); got != tc.wantErr {
			t.Errorf("%s %s: err = %v, want ErrOutputLimit=%v", tc.stop, tc.content, err, tc.wantErr)
		}
		if err != nil && isRetryable(err) {
			t.Errorf("%s: the output limit must not be retried", tc.stop)
		}
	}
}

// Temperature: an explicit 0 is sent (TemperatureSet), values are clamped to
// Claude's [0, 1], an unset 0 stays omitted, and models that reject sampling
// never get one.
func TestClaude_TemperatureWire(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "hi"}}
	for _, tc := range []struct {
		name string
		req  Request
		want any // nil = omitted
	}{
		{"explicit zero", Request{Model: "claude-sonnet-4-6", Temperature: 0, TemperatureSet: true, Messages: msgs}, 0.0},
		{"unset zero", Request{Model: "claude-sonnet-4-6", Messages: msgs}, nil},
		{"legacy non-zero", Request{Model: "claude-sonnet-4-6", Temperature: 0.4, Messages: msgs}, 0.4},
		{"clamped high", Request{Model: "claude-sonnet-4-6", Temperature: 1.7, TemperatureSet: true, Messages: msgs}, 1.0},
		{"clamped low", Request{Model: "claude-haiku-4-5", Temperature: -0.5, TemperatureSet: true, Messages: msgs}, 0.0},
		{"sampling removed", Request{Model: "claude-opus-4-8", Temperature: 0, TemperatureSet: true, Messages: msgs}, nil},
		{"unversioned mythos", Request{Model: "claude-mythos-preview", Temperature: 0.2, TemperatureSet: true, Messages: msgs}, nil},
	} {
		body := captureClaudeBody(t, tc.req)
		got, present := body["temperature"]
		switch {
		case tc.want == nil && present:
			t.Errorf("%s: temperature %v sent, want it omitted", tc.name, got)
		case tc.want != nil && (!present || got != tc.want):
			t.Errorf("%s: temperature = %v (present %v), want %v", tc.name, got, present, tc.want)
		}
	}
}

// The Fable and Mythos families reject sampling whether or not the name
// carries a version.
func TestClaudeRemovesSampling_UnversionedFamilies(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-mythos-preview": true,
		"Claude-Mythos-Preview": true,
		"prod-fable":            true,
		"claude-fable-5-1":      true,
		"claude-sonnet-4-6":     false,
		"claude-haiku-4-5":      false,
		"fabled-assistant":      false,
		"prod-claude":           false,
	} {
		if got := claudeRemovesSampling(model); got != want {
			t.Errorf("claudeRemovesSampling(%q) = %v, want %v", model, got, want)
		}
	}
}

// A tool result the tool reported as a failure ("error: …") carries
// is_error on the wire; a normal result does not.
func TestToClaudeMessages_ToolErrorSetsIsError(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Name: "get_block", Input: json.RawMessage(`{}`)}, {ID: "b", Name: "search_flow", Input: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "a", Content: "error: blockId is required"},
		{Role: "tool", ToolCallID: "b", Content: "3 matches"},
	}
	raw, _ := json.Marshal(toClaudeMessages(msgs, "claude")[2].Content)
	want := `[{"type":"tool_result","tool_use_id":"a","content":"error: blockId is required","is_error":true},` +
		`{"type":"tool_result","tool_use_id":"b","content":"3 matches"}]`
	if !jsonEqual(t, string(raw), want) {
		t.Errorf("tool results = %s, want %s", raw, want)
	}
}

// Both correlation ids — Anthropic's request-id and API Management's
// apim-request-id — reach the error text when both are present.
func TestClaude_ErrorCarriesBothRequestIDs(t *testing.T) {
	p, _ := newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req_011")
		w.Header().Set("apim-request-id", "apim-42")
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "messages: bad"}})
	})
	req := Request{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "x"}}}
	for what, err := range claudeCallBoth(p, req) {
		if err == nil || !strings.Contains(err.Error(), "request id req_011") || !strings.Contains(err.Error(), "apim-request-id apim-42") {
			t.Errorf("%s: error = %v, want both request ids", what, err)
		}
	}
}

// The Anthropic route is documented on <resource>.services.ai.azure.com only:
// a document naming the same resource's openai / cognitiveservices host is
// sent there; any other allow-listed host is kept.
func TestClaudeOnFoundryBase(t *testing.T) {
	for endpoint, want := range map[string]string{
		"https://my-res.openai.azure.com":                  "https://my-res.services.ai.azure.com",
		"https://my-res.cognitiveservices.azure.com":       "https://my-res.services.ai.azure.com",
		"https://My-Res.OpenAI.Azure.com":                  "https://my-res.services.ai.azure.com",
		"https://my-res.services.ai.azure.com":             "https://my-res.services.ai.azure.com",
		"https://apim.contoso.example":                     "https://apim.contoso.example",
		"https://my-res.privatelink.openai.azure.com":      "https://my-res.privatelink.openai.azure.com",
		"http://127.0.0.1:8080":                            "http://127.0.0.1:8080",
		"https://my-res.openai.azure.com:8443":             "https://my-res.openai.azure.com:8443",
		"https://gateway.example.com/openai.azure.com-ish": "https://gateway.example.com/openai.azure.com-ish",
	} {
		if got := claudeOnFoundryBase(endpoint); got != want {
			t.Errorf("claudeOnFoundryBase(%q) = %q, want %q", endpoint, got, want)
		}
	}
	p := newClaudeOnFoundry(AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeAPIKey, Key: "k"}}, nil, nil, nil)
	if p.endpoint() != "https://my-res.services.ai.azure.com/anthropic/v1/messages" {
		t.Errorf("messages URL = %q", p.endpoint())
	}
	if p.replayOwner() != azureFoundryProviderID {
		t.Errorf("the Foundry route replays records tagged %q, want %q", p.replayOwner(), azureFoundryProviderID)
	}
}

// Claude on Foundry: the chat service tags a turn with the Azure provider's
// ID, and the route replays it verbatim on the next request.
func TestAzureClaude_ReplaysRecordTaggedWithAzureID(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = io.WriteString(w, claudeToolTurnSSE("sig-F"))
			return
		}
		_, _ = io.WriteString(w, claudeFinalTurnSSE)
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })
	tools := []ToolDefinition{{Name: "search_flow", Description: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	req := Request{Model: "claude:my-claude", Tools: tools, Messages: []Message{{Role: "user", Content: "find xav"}}}
	var first Chunk
	if err := p.Stream(context.Background(), req, func(c Chunk) {
		if c.Done {
			first = c
		}
	}); err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	if len(first.ProviderItems) == 0 {
		t.Fatal("no record on the Done chunk")
	}
	req.Messages = append(req.Messages,
		Message{Role: "assistant", Content: "Let me search.", ToolCalls: first.ToolCalls, ProviderItems: first.ProviderItems, ProviderItemsFrom: p.ID()},
		Message{Role: "tool", ToolCallID: "toolu_1", Content: "1 match"})
	if err := p.Stream(context.Background(), req, func(Chunk) {}); err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	msgs, _ := srv.requests()[1].Body["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	got, _ := json.Marshal(assistant["content"])
	if !jsonEqual(t, string(got), string(first.ProviderItems)) {
		t.Errorf("replayed turn = %s, want the record %s", got, first.ProviderItems)
	}
}

// reasoning_content (DeepSeek, GLM, … on Azure AI Foundry) is reasoning:
// delivered as Chunk.Reasoning, never as answer text.
func TestParseOpenAISSE_ReasoningContentNeverText(t *testing.T) {
	sse := "data: {\"model\":\"DeepSeek-R1-0528\",\"choices\":[{\"delta\":{\"reasoning_content\":\"Let me think\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\" harder.\",\"content\":\"\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"Answer.\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var text, reasoning string
	var done Chunk
	if err := parseOpenAISSE(strings.NewReader(sse), func(c Chunk) {
		text += c.Text
		reasoning += c.Reasoning
		if c.Done {
			done = c
		}
	}); err != nil {
		t.Fatalf("parseOpenAISSE: %v", err)
	}
	if text != "Answer." {
		t.Errorf("text = %q, want only the answer", text)
	}
	if reasoning != "Let me think harder." {
		t.Errorf("reasoning = %q", reasoning)
	}
	if !done.Done || done.FinishReason != "stop" {
		t.Errorf("done = %+v", done)
	}
}
