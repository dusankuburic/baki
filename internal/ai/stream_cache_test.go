package ai

import (
	"strings"
	"testing"
)

// TestClaudeSSE_CacheTokensCounted pins R1: Claude's input_tokens EXCLUDES
// cache reads/writes, which are billed separately — not folding them in made
// prompt-cached conversations (the norm on long tool loops) silently bypass
// the daily budget on the cached slice.
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
}
