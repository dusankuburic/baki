package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestParseOpenAISSE_ToolCallsWithoutIndex (D-28): fragments keyed by `id`
// alone (no `index`) get their own slots, and an id-less, index-less
// continuation appends to the last slot — instead of every call merging into
// slot 0 with concatenated argument JSON.
func TestParseOpenAISSE_ToolCallsWithoutIndex(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"name":"get_block","arguments":"{\"id\":1}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"name":"list_findings","arguments":""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	var done Chunk
	if err := parseOpenAISSE(strings.NewReader(body), func(c Chunk) {
		if c.Done {
			done = c
		}
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(done.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v, want 2", done.ToolCalls)
	}
	a, b := done.ToolCalls[0], done.ToolCalls[1]
	if a.ID != "call_a" || a.Name != "get_block" || string(a.Input) != `{"id":1}` {
		t.Errorf("first call = %+v", a)
	}
	if b.ID != "call_b" || b.Name != "list_findings" || string(b.Input) != `{}` {
		t.Errorf("second call = %+v", b)
	}

	// Indexed fragments are unchanged, and an id seen again resolves to its slot.
	idx0, idx1 := 0, 1
	blocks := map[int]*pendingToolCall{}
	byID := map[string]int{}
	if s := toolCallSlot(&idx1, "x", byID, blocks, -1); s != 1 {
		t.Errorf("index wins: got %d", s)
	}
	if s := toolCallSlot(&idx0, "", byID, blocks, 3); s != 0 {
		t.Errorf("index wins over last: got %d", s)
	}
	blocks[0], blocks[2] = &pendingToolCall{}, &pendingToolCall{}
	if s := toolCallSlot(nil, "new", byID, blocks, 2); s != 3 {
		t.Errorf("a new id takes the next free slot: got %d", s)
	}
	byID["new"] = 3
	if s := toolCallSlot(nil, "new", byID, blocks, 0); s != 3 {
		t.Errorf("a known id keeps its slot: got %d", s)
	}
	if s := toolCallSlot(nil, "", byID, blocks, -1); s != 0 {
		t.Errorf("nothing known: slot 0, got %d", s)
	}
}

// TestParseOpenAISSE_ContentFilterCarriesUsage (D-31): the content-filter
// stop is emitted at the END of the stream, carrying the usage and model the
// trailing usage-only chunk reported — so the blocked completion is metered,
// not estimated — with no Done and no keepalive after the text.
func TestParseOpenAISSE_ContentFilterCarriesUsage(t *testing.T) {
	body := `data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":50000,"completion_tokens":7},"model":"gpt-4o-2024-11-20"}` + "\n\n" +
		"data: [DONE]\n\n"
	var chunks []Chunk
	if err := parseOpenAISSE(strings.NewReader(body), func(c Chunk) { chunks = append(chunks, c) }); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chunks) != 2 || chunks[0].Text != "partial" {
		t.Fatalf("chunks = %+v, want the text then the error (no keepalive in between)", chunks)
	}
	last := chunks[1]
	if !errors.Is(last.Error, ErrContentFiltered) || last.TokensIn != 50000 || last.TokensOut != 7 || last.ServedModel != "gpt-4o-2024-11-20" {
		t.Errorf("error chunk = %+v, want ErrContentFiltered carrying 50000/7 and the served model", last)
	}
	for _, c := range chunks {
		if c.Done {
			t.Error("a filtered completion must not end with a Done chunk")
		}
	}

	// A stream cut after the finish (no usage, no [DONE]) still reports the
	// verdict rather than a truncation.
	cut := `data: {"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}` + "\n\n"
	var cutErr error
	if err := parseOpenAISSE(strings.NewReader(cut), func(c Chunk) {
		if c.Error != nil {
			cutErr = c.Error
		}
	}); err != nil || !errors.Is(cutErr, ErrContentFiltered) {
		t.Errorf("cut stream: err=%v chunk error=%v", err, cutErr)
	}

	// Through the audited wrapper: the recorded usage is the reported one.
	stub := &sseStub{body: body}
	sink := newUsageSink()
	ap := NewAuditedProvider(stub, sink.recorder(), "u1", "stub")
	if err := ap.Stream(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}, func(Chunk) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if m := sink.next(t); m.PromptTokens != 50000 || m.CompletionTokens != 7 {
		t.Errorf("recorded %d/%d, want the reported 50000/7 (not an estimate)", m.PromptTokens, m.CompletionTokens)
	}
}

// sseStub streams a fixed OpenAI SSE body through the real parser.
type sseStub struct {
	auditStub
	body string
}

func (s *sseStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	return parseOpenAISSE(strings.NewReader(s.body), onChunk)
}

func (s *sseStub) Models(context.Context) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m"}}, nil
}

// TestParseOpenAISSE_MidStreamErrorEventIsClassified (PERF-10): a
// `data: {"error":{…}}` event after the 200 ends the stream with a classified
// error chunk (retryable for a server error, so a failure before any content
// is retried and counted by the breaker) carrying the provider's message —
// not with a "truncated" error that dropped it.
func TestParseOpenAISSE_MidStreamErrorEventIsClassified(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
		is    error
	}{
		{"server error", `{"error":{"message":"The server had an error while processing your request","type":"server_error"}}`, ErrProviderDown},
		{"rate limit", `{"error":{"message":"Requests to the ChatCompletions_Create Operation have exceeded token rate limit","code":"rate_limit_exceeded"}}`, ErrRateLimited},
		{"content filter", `{"error":{"message":"filtered","code":"content_filter"}}`, ErrContentFiltered},
		{"other", `{"error":{"message":"deployment is being updated","code":"DeploymentUpdating"}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n" +
				"data: " + tc.event + "\n\n"
			var chunks []Chunk
			if err := parseOpenAISSE(strings.NewReader(body), func(c Chunk) { chunks = append(chunks, c) }); err != nil {
				t.Fatalf("the error travels as a chunk; parse returned %v", err)
			}
			if len(chunks) != 2 || chunks[0].Text != "partial" || chunks[1].Error == nil {
				t.Fatalf("chunks = %+v, want the text then the error", chunks)
			}
			got := chunks[1].Error
			if tc.is != nil && !errors.Is(got, tc.is) {
				t.Errorf("error = %v, want %v", got, tc.is)
			}
			if tc.is == nil && (errors.Is(got, ErrProviderDown) || errors.Is(got, ErrRateLimited) || !strings.Contains(got.Error(), "deployment is being updated")) {
				t.Errorf("unknown error must stay plain and keep the message: %v", got)
			}
			if chunks[1].ServedModel != "gpt-4o" {
				t.Errorf("the error chunk must carry the served model: %+v", chunks[1])
			}
		})
	}

	// Before any content, through the retry wrapper: a server error is
	// retried and the second attempt's answer is delivered.
	attempts := 0
	retrying := NewRetryingProviderWithConfig(&attemptStreamStub{fn: func(onChunk func(Chunk)) error {
		attempts++
		if attempts == 1 {
			return parseOpenAISSE(strings.NewReader(`data: {"error":{"message":"overloaded","type":"server_error"}}`+"\n\n"), onChunk)
		}
		return parseOpenAISSE(strings.NewReader(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n"), onChunk)
	}}, 2, 0)
	var text strings.Builder
	if err := retrying.Stream(context.Background(), Request{}, func(c Chunk) { text.WriteString(c.Text) }); err != nil || text.String() != "ok" || attempts != 2 {
		t.Errorf("pre-content server error: err=%v text=%q attempts=%d", err, text.String(), attempts)
	}
}

// attemptStreamStub runs fn on every Stream call.
type attemptStreamStub struct {
	auditStub
	fn func(onChunk func(Chunk)) error
}

func (s *attemptStreamStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	return s.fn(onChunk)
}

// TestParseOpenAISSE_ServedModelRidesOnContentChunks (D-40): every content
// chunk, the think flush and the keepalives carry the served model, not only
// Done, so a stream cut before Done is still priced by the model that
// answered.
func TestParseOpenAISSE_ServedModelRidesOnContentChunks(t *testing.T) {
	body := `data: {"model":"gpt-5-nano-2025-08-07","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"<thi"}}]}` + "\n\n"
	var chunks []Chunk
	err := parseOpenAISSE(strings.NewReader(body), func(c Chunk) { chunks = append(chunks, c) })
	if err == nil {
		t.Fatal("want the truncation error")
	}
	if len(chunks) < 3 {
		t.Fatalf("chunks = %+v", chunks)
	}
	for i, c := range chunks {
		if c.ServedModel != "gpt-5-nano-2025-08-07" {
			t.Errorf("chunk %d (%+v) carries no served model", i, c)
		}
	}
	if chunks[0].Text != "" || chunks[0].Done || chunks[0].Error != nil {
		t.Errorf("the role delta is still a keepalive: %+v", chunks[0])
	}
	if got := chunks[len(chunks)-1]; got.Text != "<thi" {
		t.Errorf("the flush of held bytes must be the last chunk: %+v", got)
	}
}
