package ai

import (
	"strings"
	"testing"
)

// TestClaudeSSE_CacheTokensCounted pins R1: Claude's input_tokens EXCLUDES
// cache reads/writes, which are billed separately — not folding them in made
// prompt-cached conversations (the norm on long tool loops) silently bypass
// the daily budget on the cached slice. The breakdown also rides the usage
// chunks (CacheRead/WriteTokens, included in TokensIn) so the audited
// wrapper prices the slice at the cached rates.
func TestClaudeSSE_CacheTokensCounted(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":2000,"cache_creation_input_tokens":500}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		``,
		`data: [DONE]`,
		"",
	}, "\n")

	var got Chunk
	if err := parseClaudeSSE(strings.NewReader(sse), func(c Chunk) {
		if c.Done {
			got = c
		}
	}); err != nil {
		t.Fatalf("parseClaudeSSE: %v", err)
	}
	if !got.Done {
		t.Fatal("no Done chunk")
	}
	if want := 100 + 2000 + 500; got.TokensIn != want {
		t.Errorf("TokensIn = %d, want %d (input + cache read + cache write)", got.TokensIn, want)
	}
	if got.TokensOut != 7 {
		t.Errorf("TokensOut = %d, want 7", got.TokensOut)
	}
	if got.CacheReadTokens != 2000 || got.CacheWriteTokens != 500 {
		t.Errorf("cache breakdown on Done = read %d / write %d, want 2000 / 500 (a slice of TokensIn)", got.CacheReadTokens, got.CacheWriteTokens)
	}
}

// The streaming parser caps cached_tokens at prompt_tokens, as chat() does
// — the breakdown is a slice of the total, and an upstream reporting more
// cached tokens than prompt tokens must not price tokens never sent.
func TestParseOpenAISSE_CachedTokensCappedAtPromptTokens(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"model":"gpt-4o","choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":5000}}}`,
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
	if !got.Done || got.TokensIn != 100 {
		t.Fatalf("usage chunk = %+v, want prompt_tokens 100", got)
	}
	if got.CacheReadTokens != 100 {
		t.Errorf("CacheReadTokens = %d, want 100 (capped at prompt_tokens)", got.CacheReadTokens)
	}
}
