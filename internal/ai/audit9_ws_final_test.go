package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// F-10: a blank turn in the history is not sent to the Messages API (it
// rejects empty content); the last message is always kept.
func TestToClaudeMessages_DropsBlankHistoryTurns(t *testing.T) {
	out := toClaudeMessages([]Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "  "},
		{Role: "user", Content: "second"},
	}, "")
	if len(out) != 2 || out[0].Role != "user" || out[1].Role != "user" {
		t.Fatalf("got %d messages %+v, want the blank assistant turn dropped", len(out), out)
	}
	last := toClaudeMessages([]Message{{Role: "user", Content: ""}}, "")
	if len(last) != 1 {
		t.Errorf("a lone blank final message must be kept, got %d", len(last))
	}
}

// F-05: the non-streaming chat-completions path reports the cached slice of
// the prompt, capped at the prompt size.
func TestOpenAICompatChat_ReportsCachedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4.1","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":64}}}`))
	}))
	defer srv.Close()
	url := srv.URL + "/chat/completions"
	p := &openaiBase{apiKey: "k", client: srv.Client(), baseURL: &url, providerLabel: "test"}
	resp, err := p.chat(context.Background(), Request{Model: "gpt-4.1", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.CacheReadTokens != 64 || resp.TokensIn != 100 {
		t.Errorf("CacheReadTokens=%d TokensIn=%d, want 64 of 100", resp.CacheReadTokens, resp.TokensIn)
	}
}
