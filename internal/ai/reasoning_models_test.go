package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// ---- inline <think> reasoning (DeepSeek-R1 & co.) ---------------------------

// feedThink runs deltas through one splitter and returns what each write
// released plus the final flush.
func feedThink(deltas ...string) (texts, reasonings []string, text, reasoning string) {
	var s thinkSplitter
	var tb, rb strings.Builder
	for _, d := range deltas {
		t, r := s.write(d)
		texts = append(texts, t)
		reasonings = append(reasonings, r)
		tb.WriteString(t)
		rb.WriteString(r)
	}
	t, r := s.flush()
	tb.WriteString(t)
	rb.WriteString(r)
	return texts, reasonings, tb.String(), rb.String()
}

func TestThinkSplitter(t *testing.T) {
	cases := []struct {
		name          string
		deltas        []string
		wantText      string
		wantReasoning string
	}{
		{"no think block", []string{"Hello", " world"}, "Hello world", ""},
		{"tags split across deltas", []string{"<th", "ink>\nLet me", " think</th", "ink>\n\nThe answer", " is 42."}, "The answer is 42.", "\nLet me think"},
		{"one byte at a time", strings.Split("<think>ab</think>\nhi", ""), "hi", "ab"},
		{"leading newline before the tag", []string{"\n", "<think>", "r", "</think>", "\n\n", "A"}, "A", "r"},
		{"empty block", []string{"<think>\n\n</think>\n\nAnswer"}, "Answer", "\n\n"},
		{"unterminated block at EOF", []string{"<think>abc", " def</thi"}, "", "abc def</thi"},
		{"answer starting with an HTML tag", []string{"<", "div>hi"}, "<div>hi", ""},
		{"answer starting with <th that is not think", []string{"<th", "ead>x"}, "<thead>x", ""},
		{"think tag mid-answer is text", []string{"Answer <think>x</think> done"}, "Answer <think>x</think> done", ""},
		{"leading whitespace without a tag is kept", []string{"  ", "Hi"}, "  Hi", ""},
		{"close-tag prefix that is not the tag", []string{"<think>a</t", "b</think>c"}, "c", "a</tb"},
		{"lone < at EOF is text", []string{"<"}, "<", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, text, reasoning := feedThink(tc.deltas...)
			if text != tc.wantText || reasoning != tc.wantReasoning {
				t.Errorf("text=%q reasoning=%q, want text=%q reasoning=%q", text, reasoning, tc.wantText, tc.wantReasoning)
			}
		})
	}
}

// TestThinkSplitter_BuffersAtMostOneDelta: content that does not start with
// <think> passes through untouched — released by the first delta that rules
// the tag out, never held for the rest of the answer.
func TestThinkSplitter_BuffersAtMostOneDelta(t *testing.T) {
	texts, _, _, _ := feedThink("Hello", " world")
	if texts[0] != "Hello" || texts[1] != " world" {
		t.Errorf("plain content was buffered: %q", texts)
	}
	texts, _, _, _ = feedThink("<", "div>", "x")
	if texts[0] != "" || texts[1] != "<div>" || texts[2] != "x" {
		t.Errorf("a leading '<' must be held for one delta only: %q", texts)
	}
	// Inside the block, reasoning is released as it streams (only a possible
	// closing-tag prefix is held back).
	_, reasonings, _, _ := feedThink("<think>step one", " step two<", "/think>A")
	if reasonings[0] != "step one" || reasonings[1] != " step two" || reasonings[2] != "" {
		t.Errorf("reasoning released = %q", reasonings)
	}
}

// TestThinkSplitter_WhitespaceRunIsBounded: while only whitespace arrives
// the splitter must hold it (it could still precede "<think>"), but no
// longer without bound — a whitespace-only stream used to accumulate every
// byte in pending. Past the cap the run is released as answer text, as the
// non-streaming split does, and the splitter passes through.
func TestThinkSplitter_WhitespaceRunIsBounded(t *testing.T) {
	ws := strings.Repeat(" ", 256)
	var s thinkSplitter
	peak := 0
	var text strings.Builder
	for range 64 { // 16 KiB of whitespace deltas
		tOut, _ := s.write(ws)
		text.WriteString(tOut)
		// The cap is checked after the append, so pending may overshoot by one
		// delta; it must never hold the whole run.
		peak = max(peak, len(s.pending))
	}
	tOut, _ := s.write("Answer")
	text.WriteString(tOut)
	flushed, _ := s.flush()

	if peak > thinkHoldMax+len(ws) {
		t.Errorf("pending peaked at %d bytes; the whitespace run was held, not bounded", peak)
	}
	if s.state != thinkPassthrough {
		t.Errorf("state = %v, want passthrough after the whitespace cap", s.state)
	}
	if got := text.String() + flushed; got != strings.Repeat(" ", 64*256)+"Answer" {
		t.Errorf("whitespace must pass through as answer text once the cap trips, got %d bytes ending %q", len(got), got[len(got)-8:])
	}
}

func TestSplitAndStripLeadingThink(t *testing.T) {
	if text, reasoning := splitLeadingThink("<think>plan</think>\n\nAnswer"); text != "Answer" || reasoning != "plan" {
		t.Errorf("splitLeadingThink = %q, %q", text, reasoning)
	}
	if text, reasoning := splitLeadingThink("<think>cut off while thinking"); text != "" || reasoning != "cut off while thinking" {
		t.Errorf("unterminated: %q, %q", text, reasoning)
	}
	if text, reasoning := splitLeadingThink("plain <think>"); text != "plain <think>" || reasoning != "" {
		t.Errorf("no leading block: %q, %q", text, reasoning)
	}
	for in, want := range map[string]string{
		"<think>x</think>\nAnswer":        "Answer",
		"\n<think>x</think>Answer":        "Answer",
		"<think>never closed":             "<think>never closed",
		"<think>only reasoning</think>\n": "<think>only reasoning</think>\n",
		"Answer <think>x</think>":         "Answer <think>x</think>",
	} {
		if got := stripLeadingThink(in); got != want {
			t.Errorf("stripLeadingThink(%q) = %q, want %q", in, got, want)
		}
	}
}

// sseBody frames JSON events as an SSE body ending in [DONE].
func sseBody(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func contentEvent(s string) string {
	j, _ := json.Marshal(s)
	return `{"model":"DeepSeek-R1-0528","choices":[{"index":0,"delta":{"content":` + string(j) + `},"finish_reason":null}]}`
}

func finishEvent(reason string) string {
	return `{"choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}],"usage":{"prompt_tokens":9,"completion_tokens":40}}`
}

func collectSSE(t *testing.T, body string) (chunks []Chunk, text, reasoning string, done Chunk, err error) {
	t.Helper()
	var tb, rb strings.Builder
	err = parseOpenAISSE(strings.NewReader(body), func(c Chunk) {
		chunks = append(chunks, c)
		tb.WriteString(c.Text)
		rb.WriteString(c.Reasoning)
		if c.Done {
			done = c
		}
	})
	return chunks, tb.String(), rb.String(), done, err
}

func TestParseOpenAISSE_ThinkBlockSplitAcrossEvents(t *testing.T) {
	body := sseBody(
		contentEvent("<th"), contentEvent("ink>\nThe user wants"), contentEvent(" a sum.</th"),
		contentEvent("ink>\n\n"), contentEvent("It is "), contentEvent("4."), finishEvent("stop"))
	chunks, text, reasoning, done, err := collectSSE(t, body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if text != "It is 4." {
		t.Errorf("answer text = %q, want only the text after </think>", text)
	}
	if reasoning != "\nThe user wants a sum." {
		t.Errorf("reasoning = %q", reasoning)
	}
	if !done.Done || done.FinishReason != "stop" || done.TokensOut != 40 || done.ServedModel != "DeepSeek-R1-0528" {
		t.Errorf("done = %+v", done)
	}
	for _, c := range chunks {
		if strings.Contains(c.Text, "think") || strings.Contains(c.Text, "user wants") {
			t.Errorf("reasoning leaked into answer text: %+v", c)
		}
	}
}

func TestParseOpenAISSE_NoThinkBlockPassesThrough(t *testing.T) {
	_, text, reasoning, done, err := collectSSE(t, sseBody(contentEvent("<"), contentEvent("b>bold</b> answer"), finishEvent("stop")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if text != "<b>bold</b> answer" || reasoning != "" || !done.Done {
		t.Errorf("text=%q reasoning=%q done=%+v", text, reasoning, done)
	}
}

// TestParseOpenAISSE_UnterminatedThinkAtEOF: an answer cut at the token cap
// while the model was still thinking carries no answer text — the reasoning
// stays reasoning (the chat service then reports the exhausted budget).
func TestParseOpenAISSE_UnterminatedThinkAtEOF(t *testing.T) {
	_, text, reasoning, done, err := collectSSE(t, sseBody(contentEvent("<think>Let me"), contentEvent(" consider</"), finishEvent("length")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if text != "" {
		t.Errorf("text = %q, want none", text)
	}
	if reasoning != "Let me consider</" {
		t.Errorf("reasoning = %q (held bytes must be flushed before Done)", reasoning)
	}
	if !done.Done || done.FinishReason != "length" {
		t.Errorf("done = %+v", done)
	}
}

// TestParseOpenAISSE_HeldBytesFlushedBeforeTruncationError: a stream cut
// without a terminal marker still delivers what the splitter held.
func TestParseOpenAISSE_HeldBytesFlushedBeforeTruncationError(t *testing.T) {
	var text strings.Builder
	err := parseOpenAISSE(strings.NewReader("data: "+contentEvent("<")+"\n\n"), func(c Chunk) { text.WriteString(c.Text) })
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want truncation", err)
	}
	if text.String() != "<" {
		t.Errorf("text = %q", text.String())
	}
}

// ---- keepalives for events without text (#33) --------------------------------

// isKeepalive reports an empty chunk (Chunk holds a slice, so it is not
// comparable with ==).
func isKeepalive(c Chunk) bool {
	return c.Text == "" && c.Reasoning == "" && !c.Done && c.Error == nil && len(c.ToolCalls) == 0 &&
		c.ToolEvent == nil && !c.ServerStep && c.TokensIn == 0 && c.TokensOut == 0 && c.FinishReason == "" && c.ServedModel == ""
}

func reasoningContentEvent(s string) string {
	j, _ := json.Marshal(s)
	return `{"choices":[{"index":0,"delta":{"reasoning_content":` + string(j) + `},"finish_reason":null}]}`
}

// TestParseOpenAISSE_ReasoningContentKeepalives: DeepSeek-V3.2/V4 and others
// stream their thinking in delta.reasoning_content for minutes before the
// first content delta. Each such event must reach the caller as one
// reasoning-only chunk (it resets the idle watchdog like a keepalive),
// never as text.
func TestParseOpenAISSE_ReasoningContentKeepalives(t *testing.T) {
	const n = 5
	events := make([]string, 0, n+2)
	for i := 0; i < n; i++ {
		events = append(events, reasoningContentEvent("thinking step "))
	}
	events = append(events, contentEvent("Answer."), finishEvent("stop"))
	chunks, text, reasoning, _, err := collectSSE(t, sseBody(events...))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	firstText := -1
	for i, c := range chunks {
		if c.Text != "" {
			firstText = i
			break
		}
	}
	if firstText < n {
		t.Fatalf("got %d chunk(s) before the first text, want at least %d keepalives: %+v", firstText, n, chunks)
	}
	for _, c := range chunks[:firstText] {
		if c.Reasoning != "thinking step " || c.Text != "" || c.Done || c.Error != nil || len(c.ToolCalls) != 0 ||
			c.FinishReason != "" || c.TokensIn != 0 || c.TokensOut != 0 {
			t.Errorf("reasoning event must yield one reasoning-only chunk, got %+v", c)
		}
	}
	if text != "Answer." || reasoning != strings.Repeat("thinking step ", n) {
		t.Errorf("text=%q reasoning=%q — reasoning_content must never become text", text, reasoning)
	}
	doneCount := 0
	for _, c := range chunks {
		if c.Done {
			doneCount++
		}
	}
	if doneCount != 1 || !chunks[len(chunks)-1].Done {
		t.Errorf("want exactly one Done, last: %d Done chunk(s) in %+v", doneCount, chunks)
	}
}

// TestParseOpenAISSE_OneChunkPerDecodedEvent: every decoded event yields
// exactly one chunk — its content, or one keepalive (tool-call fragments, the
// finish event, a usage-only event) — and [DONE] adds the Done chunk.
func TestParseOpenAISSE_OneChunkPerDecodedEvent(t *testing.T) {
	body := sseBody(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search_flow","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":1}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`,
	)
	chunks, _, _, done, err := collectSSE(t, body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chunks) != 5 {
		t.Fatalf("got %d chunks, want 4 keepalives + Done: %+v", len(chunks), chunks)
	}
	for _, c := range chunks[:4] {
		if !isKeepalive(c) {
			t.Errorf("want keepalive, got %+v", c)
		}
	}
	if len(done.ToolCalls) != 1 || string(done.ToolCalls[0].Input) != `{"q":1}` || done.TokensIn != 3 || done.TokensOut != 4 {
		t.Errorf("done = %+v", done)
	}
}

// TestAudited_ReasoningCountsTowardCancelledEstimate: a stream cancelled while
// the model is still thinking consumed (billed) output — the split-off
// reasoning feeds the estimate like text does; keepalives add nothing.
func TestAudited_ReasoningCountsTowardCancelledEstimate(t *testing.T) {
	stub := &auditStub{
		models:       []ModelInfo{{ID: "m"}},
		streamChunks: []Chunk{{}, {Reasoning: strings.Repeat("r", 400)}, {}},
		streamErr:    context.Canceled,
	}
	recorded := make(chan *interfaces.UsageMetric, 1)
	ap := NewAuditedProvider(stub, func(_ context.Context, m *interfaces.UsageMetric) error {
		recorded <- m
		return nil
	}, "user-1", "stub")
	var sawText bool
	_ = ap.Stream(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "prompt"}}}, func(c Chunk) {
		if c.Text != "" {
			sawText = true
		}
	})
	if sawText {
		t.Error("reasoning surfaced as text")
	}
	select {
	case m := <-recorded:
		if m.CompletionTokens != 100 {
			t.Errorf("completion tokens = %d, want the reasoning estimate 100", m.CompletionTokens)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stream cancelled mid-reasoning recorded no usage")
	}
}

// TestRetry_Stream_KeepalivesAndReasoningDoNotBlockReplay: keepalives and
// split-off reasoning are invisible to the user, so a retryable failure after
// them still replays the request.
func TestRetry_Stream_KeepalivesAndReasoningDoNotBlockReplay(t *testing.T) {
	stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{})
			onChunk(Chunk{Reasoning: "thinking"})
			return ErrProviderDown
		},
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{Text: "ok"})
			onChunk(Chunk{Done: true})
			return nil
		},
	}}
	rp := NewRetryingProviderWithConfig(stub, 3, time.Millisecond)
	var text string
	if err := rp.Stream(context.Background(), Request{}, func(c Chunk) { text += c.Text }); err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if text != "ok" || stub.streamCall != 2 {
		t.Errorf("text=%q calls=%d, want a replay", text, stub.streamCall)
	}
}

func TestToOpenAIMessages_StripsStoredThinkBlock(t *testing.T) {
	msgs := toOpenAIMessages("", []Message{
		{Role: "user", Content: "<think>user text stays</think> q"},
		{Role: "assistant", Content: "<think>old reasoning</think>\n\nold answer"},
	})
	if msgs[0].Content != "<think>user text stays</think> q" {
		t.Errorf("user message altered: %q", msgs[0].Content)
	}
	if msgs[1].Content != "old answer" {
		t.Errorf("stored assistant reasoning re-sent: %q", msgs[1].Content)
	}
}

func TestAzureFoundry_ChatSplitsThinkBlock(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 200, chatCompletionJSON("<think>\nadd them\n</think>\n\nThe sum is 4.", "DeepSeek-R1-0528"))
	})
	p := newAzureKeyProvider(t, srv, nil)
	resp, err := p.Chat(context.Background(), Request{Model: "r1", Messages: []Message{{Role: "user", Content: "2+2?"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "The sum is 4." || resp.Reasoning != "\nadd them\n" {
		t.Errorf("content=%q reasoning=%q", resp.Content, resp.Reasoning)
	}
}

// ---- gpt-5.6: function tools need reasoning_effort "none" (#29) --------------

const gpt56ToolsRejection = "Function tools with reasoning_effort are not supported for gpt-5.6-sol in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'."

// gpt56Server behaves like a gpt-5.6 deployment on chat completions: tools
// are refused unless reasoning_effort is "none"; with rejectTemperature it
// also refuses a non-default temperature. gpt56ResponsesStream is the
// resource's /openai/v1/responses answer for the same deployment: reasoning
// with its encrypted content and one function call, as the reroute expects.
const gpt56ResponsesStream = `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_56","type":"reasoning","encrypted_content":"ENC56"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_56","type":"reasoning","encrypted_content":"ENC56"}}

data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"search_flow","arguments":"{\"q\":\"x\"}"}}

data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"search_flow","arguments":"{\"q\":\"x\"}"}}

data: {"type":"response.completed","response":{"status":"completed","model":"gpt-5.6-sol","usage":{"input_tokens":11,"output_tokens":3}}}

`

// gpt56ResponsesServer answers the Responses route and the chat-completions
// route as a gpt-5.6 deployment: chat completions refuse tools unless
// reasoning_effort is "none"; the Responses route answers normally.
func gpt56Server(t *testing.T, rejectTemperature, temperatureFirst bool) *azureTestServer {
	return newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, body map[string]any) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(gpt56ResponsesStream))
			return
		}
		temperature := func() bool {
			if _, has := body["temperature"]; has && rejectTemperature {
				writeJSON(w, 400, map[string]any{"error": map[string]any{
					"message": "Unsupported value: 'temperature' does not support 0.3 with this model. Only the default (1) value is supported.",
					"type":    "invalid_request_error", "param": "temperature", "code": "unsupported_value"}})
				return true
			}
			return false
		}
		tools := func() bool {
			if _, has := body["tools"]; has && body["reasoning_effort"] != "none" {
				writeJSON(w, 400, map[string]any{"error": map[string]any{
					"message": gpt56ToolsRejection, "type": "invalid_request_error", "param": "reasoning_effort", "code": nil}})
				return true
			}
			return false
		}
		if temperatureFirst {
			if temperature() || tools() {
				return
			}
		} else if tools() || temperature() {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(azureStreamFixture))
	})
}

func toolsRequest(temperature float64) Request {
	return Request{Model: "gpt56", Temperature: temperature, Messages: []Message{{Role: "user", Content: "fix it"}},
		Tools: []ToolDefinition{{Name: "search_flow", InputSchema: []byte(`{"type":"object"}`)}}}
}

// The observed chat-completions tools-with-reasoning rejection is
// rerouted through the resource's Responses route — the turn keeps its
// tools AND its reasoning (no reasoning_effort "none" repair at all).
func TestAzureFoundry_ReasoningToolsRejectionReroutesToResponses(t *testing.T) {
	srv := gpt56Server(t, false, false)
	p := newAzureKeyProvider(t, srv, nil)
	var done Chunk
	err := p.Stream(context.Background(), toolsRequest(0), func(c Chunk) {
		if c.Done {
			done = c
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v (ErrToolsUnsupported=%v)", err, errors.Is(err, ErrToolsUnsupported))
	}
	if len(done.ToolCalls) != 1 || done.ToolCalls[0].Name != "search_flow" {
		t.Errorf("tool calls = %+v", done.ToolCalls)
	}
	if !strings.Contains(string(done.ProviderItems), `"reasoning"`) {
		t.Errorf("the rerouted turn must keep its reasoning for replay, got %s", done.ProviderItems)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want the rejected chat-completions one and the Responses reroute", len(reqs))
	}
	if !strings.HasSuffix(reqs[0].Path, "/chat/completions") || reqs[0].Body["reasoning_effort"] != nil {
		t.Errorf("first request = %s %v", reqs[0].Path, reqs[0].Body)
	}
	if !strings.HasSuffix(reqs[1].Path, "/openai/v1/responses") {
		t.Fatalf("rerouted request path = %s", reqs[1].Path)
	}
	if reqs[1].Body["reasoning_effort"] != nil || reqs[1].Body["reasoning"] != nil || reqs[1].Body["agent_reference"] != nil {
		t.Errorf("rerouted request must carry no effort, no reasoning object and no agent_reference: %v", reqs[1].Body)
	}
	if reqs[1].Body["store"] != false || reqs[1].Body["tools"] == nil {
		t.Errorf("rerouted request must be store:false with the tools kept: %v", reqs[1].Body)
	}
	// The route memo now answers later turns directly on /responses.
	if err := p.Stream(context.Background(), toolsRequest(0), func(Chunk) {}); err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	reqs = srv.requests()
	if len(reqs) != 3 || !strings.HasSuffix(reqs[2].Path, "/openai/v1/responses") {
		t.Errorf("memoised turn: %d requests, last %s", len(reqs), reqs[len(reqs)-1].Path)
	}
}

// TestAzureFoundry_TemperatureAndReasoningRepairsKeepTools: gpt-5.6 refuses
// temperature AND tools-with-reasoning; each parameter gets its own repair,
// so the turn succeeds with its tools in 3 requests (either order).
func TestAzureFoundry_TemperatureAndReasoningRepairsKeepTools(t *testing.T) {
	for _, temperatureFirst := range []bool{true, false} {
		srv := gpt56Server(t, true, temperatureFirst)
		p := newAzureKeyProvider(t, srv, nil)
		var done Chunk
		if err := p.Stream(context.Background(), toolsRequest(0.3), func(c Chunk) {
			if c.Done {
				done = c
			}
		}); err != nil {
			t.Fatalf("temperatureFirst=%v: Stream: %v", temperatureFirst, err)
		}
		reqs := srv.requests()
		// temperatureFirst: temperature repair, tools rejection, reroute (3);
		// tools-first: the rejection reroutes before temperature is ever
		// seen (2 — the Responses route answers whatever sampling is left).
		want := map[bool]int{true: 3, false: 2}[temperatureFirst]
		if len(reqs) != want {
			t.Fatalf("temperatureFirst=%v: got %d requests, want %d", temperatureFirst, len(reqs), want)
		}
		last := reqs[len(reqs)-1]
		if !strings.HasSuffix(last.Path, "/openai/v1/responses") {
			t.Fatalf("temperatureFirst=%v: final request path = %s", temperatureFirst, last.Path)
		}
		if _, has := last.Body["temperature"]; has || last.Body["tools"] == nil {
			t.Errorf("temperatureFirst=%v: final request = %v", temperatureFirst, last.Body)
		}
		if len(done.ToolCalls) != 1 {
			t.Errorf("temperatureFirst=%v: tools lost: %+v", temperatureFirst, done)
		}
	}
}

// TestAzureFoundry_ReasoningNoneRefusedDegradesToToolsUnsupported: a model
// that refuses tools with reasoning AND refuses reasoning_effort "none" is
// reported as tools-unsupported (the tool loop answers without tools) after
// one repair, never looped on.
func TestAzureFoundry_ReasoningNoneRefusedDegradesToToolsUnsupported(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, body map[string]any) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			// The route does not serve the model: the turn
			// falls back to chat completions' repair path.
			writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "NotFound", "message": "The API version you requested is not yet supported"}})
			return
		}
		if body["reasoning_effort"] == "none" {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"message": "Unsupported value: 'reasoning_effort' does not support 'none' with this model.",
				"type":    "invalid_request_error", "param": "reasoning_effort", "code": "unsupported_value"}})
			return
		}
		writeJSON(w, 400, map[string]any{"error": map[string]any{
			"message": gpt56ToolsRejection, "type": "invalid_request_error", "param": "reasoning_effort", "code": nil}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	err := p.Stream(context.Background(), toolsRequest(0), func(Chunk) {})
	if !errors.Is(err, ErrToolsUnsupported) {
		t.Errorf("err = %v, want ErrToolsUnsupported", err)
	}
	// chat rejection, Responses-route 404, then the fallback's own rejection
	// and its refused "none" repair.
	if n := len(srv.requests()); n != 4 {
		t.Errorf("got %d requests, want 4", n)
	}
}

// TestAzureFoundry_ReasoningRepairNeedsTools: without tools in the request the
// message is not repaired (nothing to keep) and surfaces as sent.
func TestAzureFoundry_ReasoningRepairNeedsTools(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{
			"message": gpt56ToolsRejection, "type": "invalid_request_error", "param": "reasoning_effort", "code": nil}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	if _, err := p.Chat(context.Background(), Request{Model: "gpt56", Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("expected the 400 to surface")
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("got %d requests, want 1", n)
	}
}

// ---- vLLM "auto tool choice" rejection (#30) ---------------------------------

// vllmToolChoiceError is vLLM's own error object, which Azure AI Foundry
// passes through as the text of error.message.
const vllmToolChoiceError = `{"object":"error","message":"\"auto\" tool choice requires --enable-auto-tool-choice and --tool-call-parser to be set","type":"BadRequestError","param":null,"code":400}`

func TestAzureFoundry_VLLMToolsRejectionIsToolsUnsupported(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "Bad Request", "message": vllmToolChoiceError, "status": 400}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	req := toolsRequest(0)
	req.Model = "phi4mm"
	if _, err := p.Chat(context.Background(), req); !errors.Is(err, ErrToolsUnsupported) {
		t.Errorf("Chat: err = %v, want ErrToolsUnsupported", err)
	}
	if err := p.Stream(context.Background(), req, func(Chunk) {}); !errors.Is(err, ErrToolsUnsupported) {
		t.Errorf("Stream: err = %v, want ErrToolsUnsupported", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Errorf("got %d requests, want one per call (no repair)", n)
	}
}
