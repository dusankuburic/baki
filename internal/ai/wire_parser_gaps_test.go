package ai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// R-provider-33: parser telemetry gaps the wire fixtures showed.

// TestGemini_CachedContentTokensAreCacheReads: usageMetadata.cachedContentTokenCount is the cached slice of
// promptTokenCount (which includes it), reported as CacheReadTokens on the stream's Done and on Chat.
func TestGemini_CachedContentTokensAreCacheReads(t *testing.T) {
	const answer = `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1290,"candidatesTokenCount":2,"cachedContentTokenCount":1024},"modelVersion":"gemini-2.5-flash"}`
	req := Request{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "q"}}}

	t.Run("stream", func(t *testing.T) {
		p := newStubbedGemini(t, &geminiStub{answers: []func(http.ResponseWriter){geminiSSE(answer)}})
		var done Chunk
		if err := p.Stream(context.Background(), req, func(c Chunk) {
			if c.Done {
				done = c
			}
		}); err != nil {
			t.Fatal(err)
		}
		if done.TokensIn != 1290 || done.CacheReadTokens != 1024 {
			t.Errorf("Done: in=%d cacheRead=%d, want 1290/1024", done.TokensIn, done.CacheReadTokens)
		}
	})
	t.Run("chat", func(t *testing.T) {
		p := newStubbedGemini(t, &geminiStub{answers: []func(http.ResponseWriter){geminiJSON(200, answer)}})
		resp, err := p.Chat(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.TokensIn != 1290 || resp.CacheReadTokens != 1024 {
			t.Errorf("Chat: in=%d cacheRead=%d, want 1290/1024", resp.TokensIn, resp.CacheReadTokens)
		}
	})
	t.Run("never more than the prompt", func(t *testing.T) {
		chunks, err := gatherGeminiChunks(`data: {"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":64}}` + "\n\n")
		if err != nil {
			t.Fatal(err)
		}
		if got := chunks[len(chunks)-1].CacheReadTokens; got != 10 {
			t.Errorf("cacheRead=%d, want it capped at the prompt's 10", got)
		}
	})
}

// TestParseResponsesSSE_ServedModelOnEveryChunk: once a response event named the model, every chunk carries it —
// the reasoning phase's SilentStep and keepalives, hosted tool events — so a run cut while reasoning still reports it.
func TestParseResponsesSSE_ServedModelOnEveryChunk(t *testing.T) {
	input := `data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"o4-mini-2025-04-16"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}` + "\n\n" +
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"thinking"}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}` + "\n\n"
	var chunks []Chunk
	err := parseResponsesSSE(strings.NewReader(input), func(c Chunk) { chunks = append(chunks, c) })
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a cut run ends truncated, got %v", err)
	}
	if len(chunks) < 4 {
		t.Fatalf("want a chunk per event, got %+v", chunks)
	}
	for i, c := range chunks {
		if c.ServedModel != "o4-mini-2025-04-16" {
			t.Errorf("chunk %d %+v carries no served model", i+1, c)
		}
	}
}

// TestDecodeOpenAIError_NumericCode: vLLM's non-streaming error has an integer code; the strict decode failed
// and the message was lost. A 500 with it keeps the message and is provider down.
func TestDecodeOpenAIError_NumericCode(t *testing.T) {
	const body = `{"error":{"object":"error","message":"The model crashed while loading.","type":"InternalServerError","param":null,"code":500}}`
	apiErr, ok := decodeOpenAIError([]byte(body))
	if !ok || apiErr.Error.Message != "The model crashed while loading." || apiErr.Error.Code != "500" || apiErr.Error.Type != "InternalServerError" {
		t.Fatalf("decode: ok=%v %+v", ok, apiErr.Error)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")
	_, err := p.Chat(context.Background(), compatChatReq())
	if !errors.Is(err, ErrProviderDown) || !strings.Contains(err.Error(), "The model crashed while loading.") {
		t.Fatalf("want ErrProviderDown keeping the message, got %v", err)
	}
}

// TestParsers_DataWithoutSpace: the SSE field may be written "data:value" (the space is optional); every parser
// reads it like "data: value", and Claude's "event:" likewise.
func TestParsers_DataWithoutSpace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parse func(string) ([]Chunk, error)
		input string
	}{
		{"openai", gatherOpenAIChunks, `data:{"model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":"stop"}]}` + "\n\ndata:[DONE]\n\n"},
		{"gemini", gatherGeminiChunks, `data:{"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}]}` + "\n\n"},
		{"claude", func(s string) ([]Chunk, error) {
			var out []Chunk
			err := parseClaudeSSE(strings.NewReader(s), func(c Chunk) { out = append(out, c) })
			return out, err
		}, "event:content_block_delta\ndata:{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
			"event:message_delta\ndata:{\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"},
		{"responses", func(s string) ([]Chunk, error) {
			var out []Chunk
			err := parseResponsesSSE(strings.NewReader(s), func(c Chunk) { out = append(out, c) })
			return out, err
		}, `data:{"type":"response.output_text.delta","item_id":"m1","delta":"Hi"}` + "\n\n" +
			`data:{"type":"response.completed","response":{"status":"completed","model":"m"}}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks, err := tc.parse(tc.input)
			if err != nil {
				t.Fatalf("got %v", err)
			}
			var text string
			var done bool
			for _, c := range chunks {
				text += c.Text
				done = done || c.Done
			}
			if text != "Hi" || !done {
				t.Errorf("text=%q done=%v, want Hi/true (%+v)", text, done, chunks)
			}
		})
	}
}
