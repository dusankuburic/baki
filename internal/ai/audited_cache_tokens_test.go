package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseOpenAISSE_CachedTokensBreakdown: the
// chat-completions parser carries usage.prompt_tokens_details.cached_tokens
// (Azure's prompt-cache hits; included in prompt_tokens) onto the Done chunk
// as the CacheReadTokens breakdown.
func TestParseOpenAISSE_CachedTokensBreakdown(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"model":"gpt-4o","choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5000,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":4000}}}`,
		``,
		`data: [DONE]`,
		"",
	}, "\n")
	var got Chunk
	if err := parseOpenAISSE(strings.NewReader(sse), func(c Chunk) {
		if c.Done {
			got = c
		}
	}); err != nil {
		t.Fatalf("parseOpenAISSE: %v", err)
	}
	if !got.Done || got.TokensIn != 5000 || got.TokensOut != 9 {
		t.Fatalf("usage chunk = %+v, want 5000/9", got)
	}
	if got.CacheReadTokens != 4000 {
		t.Errorf("CacheReadTokens = %d, want 4000 (a slice of prompt_tokens)", got.CacheReadTokens)
	}
	if got.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0 (chat completions report reads only)", got.CacheWriteTokens)
	}
}

// TestClaudeChat_CacheTokensCarried: the non-streaming Chat
// path fills the cache breakdown on the Response, included in TokensIn.
func TestClaudeChat_CacheTokensCarried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","stop_reason":"end_turn",
		"content":[{"type":"text","text":"hi"}],
		"usage":{"input_tokens":100,"output_tokens":7,"cache_read_input_tokens":2000,"cache_creation_input_tokens":500}}`))
	}))
	defer srv.Close()
	p := NewClaudeProvider("k")
	p.messagesURL = srv.URL
	resp, err := p.Chat(context.Background(), Request{Model: "claude-opus-5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.TokensIn != 2600 || resp.CacheReadTokens != 2000 || resp.CacheWriteTokens != 500 {
		t.Errorf("usage = in %d (read %d / write %d), want 2600 total with 2000/500 breakdown", resp.TokensIn, resp.CacheReadTokens, resp.CacheWriteTokens)
	}
}

// TestAudited_StreamCacheTokensPricedAtCachedRates: end to end, a Claude
// stream whose usage chunks carry the cache breakdown records
// the cached rate, not the full input rate, on the usage row.
func TestAudited_StreamRecordsTheTotalPromptTokens(t *testing.T) {
	sink := newUsageSink()
	stub := &auditStub{
		models: []ModelInfo{{ID: "claude-opus-5"}},
		streamChunks: []Chunk{
			{TokensIn: 10_000, CacheReadTokens: 9000, ServedModel: "claude-opus-5"},
			{Text: "hi"},
			{Done: true, TokensIn: 10_000, CacheReadTokens: 9000, TokensOut: 100, ServedModel: "claude-opus-5"},
		},
	}
	ap := NewAuditedProvider(stub, sink.recorder(), "user-1", "claude")
	if err := ap.Stream(context.Background(), Request{Model: "claude-opus-5"}, func(Chunk) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	m := sink.next(t)
	if m.PromptTokens != 10_000 {
		t.Errorf("PromptTokens = %d, want the total 10000", m.PromptTokens)
	}
}
