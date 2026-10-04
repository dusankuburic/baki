package ai

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParseClaudeSSE_ContentDeltas(t *testing.T) {
	input := "event: content_block_delta\n" +
		"data: {\"delta\": {\"type\": \"text_delta\", \"text\": \"Hello\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"delta\": {\"type\": \"text_delta\", \"text\": \" World\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"usage\": {\"output_tokens\": 3}, \"delta\": {\"stop_reason\": \"end_turn\"}}\n\n"

	var chunks []string
	var done bool
	err := parseClaudeSSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			done = true
		} else if chunk.Text != "" {
			chunks = append(chunks, chunk.Text)
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("expected done signal")
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0] != "Hello" || chunks[1] != " World" {
		t.Errorf("unexpected chunks: %v", chunks)
	}
}

func TestParseOpenAISSE_ContentDeltas(t *testing.T) {
	input := "data: {\"choices\": [{\"delta\": {\"content\": \"Hi\"}}]}\n\n" +
		"data: {\"choices\": [{\"delta\": {\"content\": \" there\"}, \"finish_reason\": \"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	var chunks []string
	var done bool
	err := parseOpenAISSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			done = true
		} else if chunk.Text != "" {
			chunks = append(chunks, chunk.Text)
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("expected done signal")
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
}

func TestParseGeminiSSE_ContentDeltas(t *testing.T) {
	input := "data: {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"Bonjour\"}]}, \"finishReason\": \"STOP\"}]}\n\n"

	var chunks []string
	var done bool
	err := parseGeminiSSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			done = true
		} else if chunk.Text != "" {
			chunks = append(chunks, chunk.Text)
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("expected done signal")
	}
	if len(chunks) != 1 || chunks[0] != "Bonjour" {
		t.Errorf("unexpected chunks: %v", chunks)
	}
}

func TestParseClaudeSSE_ToolUse(t *testing.T) {
	input := "event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Let me search\"}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"search_flow\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"foo\\\"}\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"usage\":{\"output_tokens\":7},\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"

	var text string
	var done Chunk
	err := parseClaudeSSE(strings.NewReader(input), func(c Chunk) {
		if c.Done {
			done = c
		} else if c.Text != "" {
			text += c.Text
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Let me search" {
		t.Errorf("expected streamed text %q, got %q", "Let me search", text)
	}
	if len(done.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call on Done, got %d", len(done.ToolCalls))
	}
	tc := done.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Name != "search_flow" || string(tc.Input) != `{"query":"foo"}` {
		t.Errorf("unexpected tool call: id=%q name=%q input=%s", tc.ID, tc.Name, string(tc.Input))
	}
	if done.FinishReason != "tool_use" {
		t.Errorf("expected finish reason tool_use, got %q", done.FinishReason)
	}
}

func TestParseOpenAISSE_ToolCalls(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"search_flow\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"query\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"foo\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	var done Chunk
	err := parseOpenAISSE(strings.NewReader(input), func(c Chunk) {
		if c.Done {
			done = c
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(done.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call on Done, got %d", len(done.ToolCalls))
	}
	tc := done.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "search_flow" || string(tc.Input) != `{"query":"foo"}` {
		t.Errorf("unexpected tool call: id=%q name=%q input=%s", tc.ID, tc.Name, string(tc.Input))
	}
}

func TestParseClaudeSSE_Error(t *testing.T) {
	input := "event: error\n" +
		"data: {\"error\": {\"message\": \"rate limited\"}}\n\n"

	var gotErr error
	err := parseClaudeSSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Error != nil {
			gotErr = chunk.Error
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotErr == nil {
		t.Fatal("expected chunk error")
	}
}

// TestParseClaudeSSE_ErrorClassified: mid-stream error events must map onto
// the shared sentinel errors (like the HTTP-status mapping on the initial
// response) so the retry and circuit-breaker wrappers can act on them.
func TestParseClaudeSSE_ErrorClassified(t *testing.T) {
	cases := []struct {
		errType string
		want    error
	}{
		{"rate_limit_error", ErrRateLimited},
		{"overloaded_error", ErrProviderDown},
		{"api_error", ErrProviderDown},
	}
	for _, c := range cases {
		t.Run(c.errType, func(t *testing.T) {
			input := "event: error\n" +
				"data: {\"error\": {\"type\": \"" + c.errType + "\", \"message\": \"boom\"}}\n\n"
			var gotErr error
			if err := parseClaudeSSE(strings.NewReader(input), func(chunk Chunk) {
				if chunk.Error != nil {
					gotErr = chunk.Error
				}
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !errors.Is(gotErr, c.want) {
				t.Errorf("chunk error = %v, want errors.Is(%v)", gotErr, c.want)
			}
		})
	}
	// Unknown types stay plain (permanent) errors.
	if err := classifyClaudeStreamError("invalid_request_error", "bad"); isRetryable(err) {
		t.Errorf("unknown error type classified as retryable: %v", err)
	}
}

func TestParseOpenAISSE_EmptyLines(t *testing.T) {
	input := "\n\ndata: {\"choices\": [{\"delta\": {\"content\": \"test\"}}]}\n\n\n\ndata: [DONE]\n\n"

	var chunks []string
	err := parseOpenAISSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Text != "" {
			chunks = append(chunks, chunk.Text)
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "test" {
		t.Errorf("unexpected chunks: %v", chunks)
	}
}

// TestParseSSE_MalformedEventsReturnError verifies that a stream made entirely
// of undecodable data events (and no terminal marker) surfaces an error instead
// of silently looking like a clean empty response.
func TestParseSSE_MalformedEventsReturnError(t *testing.T) {
	input := "data: {not valid json\n\n" +
		"data: also: broken}\n\n"

	t.Run("claude", func(t *testing.T) {
		err := parseClaudeSSE(strings.NewReader("event: content_block_delta\n"+input), func(Chunk) {})
		if err == nil {
			t.Fatal("expected error for all-malformed claude stream")
		}
	})
	t.Run("openai", func(t *testing.T) {
		err := parseOpenAISSE(strings.NewReader(input), func(Chunk) {})
		if err == nil {
			t.Fatal("expected error for all-malformed openai stream")
		}
	})
	t.Run("gemini", func(t *testing.T) {
		err := parseGeminiSSE(strings.NewReader(input), func(Chunk) {})
		if err == nil {
			t.Fatal("expected error for all-malformed gemini stream")
		}
	})
}
func TestParseClaudeSSE_MessageStart_InputTokens(t *testing.T) {
	input := "event: message_start\n" +
		"data: {\"message\": {\"usage\": {\"input_tokens\": 42}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"usage\": {\"output_tokens\": 5}, \"delta\": {\"stop_reason\": \"end_turn\"}}\n\n"

	var tokensIn int
	err := parseClaudeSSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.TokensIn > 0 {
			tokensIn = chunk.TokensIn
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokensIn != 42 {
		t.Errorf("expected TokensIn=42, got %d", tokensIn)
	}
}

func TestParseClaudeSSE_NonTextDeltaType_Ignored(t *testing.T) {
	// A content_block_delta with type != "text_delta" should produce no text chunk.
	input := "event: content_block_delta\n" +
		"data: {\"delta\": {\"type\": \"input_json_delta\", \"partial_json\": \"{}\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"usage\": {\"output_tokens\": 0}, \"delta\": {\"stop_reason\": \"end_turn\"}}\n\n"

	var textChunks []string
	err := parseClaudeSSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Text != "" {
			textChunks = append(textChunks, chunk.Text)
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(textChunks) != 0 {
		t.Errorf("expected no text chunks for non-text delta, got %v", textChunks)
	}
}

func TestParseClaudeSSE_EOF_WithoutDone_ReturnsTruncatedError(t *testing.T) {
	// Stream ends without a `message_delta` stop_reason or `[DONE]` marker.
	// Previously the parser synthesized a Done chunk and returned nil, so a
	// network-truncated response looked indistinguishable from a clean one.
	// Now the parser returns io.ErrUnexpectedEOF so the chat service can
	// surface the truncation to the client as an error event.
	input := "event: content_block_delta\n" +
		"data: {\"delta\": {\"type\": \"text_delta\", \"text\": \"partial\"}}\n\n"

	var doneSent bool
	err := parseClaudeSSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			doneSent = true
		}
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
	if doneSent {
		t.Error("truncated stream should not emit a Done chunk — that would mask the partial response")
	}
}

func TestParseOpenAISSE_WithUsageInFinishChunk(t *testing.T) {
	// Finish chunk includes usage metadata → tokensIn and tokensOut in Done chunk.
	input := "data: {\"choices\": [{\"delta\": {\"content\": \"Hi\"}}, {\"delta\": {}, \"finish_reason\": \"stop\"}], \"usage\": {\"prompt_tokens\": 7, \"completion_tokens\": 3}}\n\n" +
		"data: [DONE]\n\n"

	var tokensIn, tokensOut int
	err := parseOpenAISSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			tokensIn = chunk.TokensIn
			tokensOut = chunk.TokensOut
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokensIn != 7 {
		t.Errorf("expected TokensIn=7, got %d", tokensIn)
	}
	if tokensOut != 3 {
		t.Errorf("expected TokensOut=3, got %d", tokensOut)
	}
}

func TestParseOpenAISSE_WithUsageInTrailingChunk(t *testing.T) {
	// With stream_options.include_usage, OpenAI sends usage in a trailing chunk
	// whose choices array is empty, AFTER the finish_reason chunk. The Done chunk
	// must still carry those tokens (previously this usage was dropped).
	input := "data: {\"choices\": [{\"delta\": {\"content\": \"Hi\"}}]}\n\n" +
		"data: {\"choices\": [{\"delta\": {}, \"finish_reason\": \"stop\"}]}\n\n" +
		"data: {\"choices\": [], \"usage\": {\"prompt_tokens\": 11, \"completion_tokens\": 5}}\n\n" +
		"data: [DONE]\n\n"

	var tokensIn, tokensOut int
	var doneCount, textCount int
	err := parseOpenAISSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			doneCount++
			tokensIn = chunk.TokensIn
			tokensOut = chunk.TokensOut
		} else if chunk.Text != "" {
			textCount++
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doneCount != 1 {
		t.Errorf("expected exactly one Done chunk, got %d", doneCount)
	}
	if textCount != 1 {
		t.Errorf("expected 1 text chunk, got %d", textCount)
	}
	if tokensIn != 11 || tokensOut != 5 {
		t.Errorf("expected TokensIn=11 TokensOut=5, got %d/%d", tokensIn, tokensOut)
	}
}

func TestParseOpenAISSE_EOF_WithoutDone_ReturnsTruncatedError(t *testing.T) {
	// Stream ends without `[DONE]` or a non-empty `finish_reason`. Same
	// rationale as the Claude truncation test above.
	input := "data: {\"choices\": [{\"delta\": {\"content\": \"hi\"}}]}\n\n"

	var doneSent bool
	err := parseOpenAISSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			doneSent = true
		}
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
	if doneSent {
		t.Error("truncated stream should not emit a Done chunk")
	}
}

func TestParseGeminiSSE_EOF_WithoutFinishReason_ReturnsTruncatedError(t *testing.T) {
	// Stream ends without any finishReason. Same rationale as Claude/OpenAI.
	input := "data: {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"partial\"}]}}]}\n\n"

	var doneSent bool
	err := parseGeminiSSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			doneSent = true
		}
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
	if doneSent {
		t.Error("truncated stream should not emit a Done chunk")
	}
}

// TestParseGeminiSSE_NonStopFinishReason_TerminalAndNotTruncation locks in
// that any non-empty FinishReason (MAX_TOKENS, SAFETY, RECITATION, OTHER) is
// treated as a clean stream end, not a truncation. Previously only "STOP" was
// considered terminal, so a token-capped response was incorrectly auto-Done'd
// / now would be wrongly reported as truncated.
func TestParseGeminiSSE_NonStopFinishReason_TerminalAndNotTruncation(t *testing.T) {
	cases := []string{"STOP", "MAX_TOKENS", "OTHER"}
	for _, reason := range cases {
		t.Run(reason, func(t *testing.T) {
			input := "data: {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"x\"}]}, \"finishReason\": \"" + reason + "\"}]}\n\n"
			var doneSent bool
			err := parseGeminiSSE(strings.NewReader(input), func(chunk Chunk) {
				if chunk.Done {
					doneSent = true
				}
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !doneSent {
				t.Errorf("finishReason=%s: expected Done chunk", reason)
			}
		})
	}
}

// TestParseGeminiSSE_FilteredFinishReason_TerminalErrorChunk refines the
// terminal contract for content-filter finishes (SAFETY, RECITATION, …): the
// stream is NOT a truncation (returns nil), but the terminal chunk is an
// Error, not a Done — otherwise a blocked response looks like a clean, empty
// success and the user gets no explanation.
func TestParseGeminiSSE_FilteredFinishReason_TerminalErrorChunk(t *testing.T) {
	for _, reason := range []string{"SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT"} {
		t.Run(reason, func(t *testing.T) {
			input := "data: {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"x\"}]}, \"finishReason\": \"" + reason + "\"}]}\n\n"
			var gotErr error
			var doneSent bool
			err := parseGeminiSSE(strings.NewReader(input), func(chunk Chunk) {
				if chunk.Error != nil {
					gotErr = chunk.Error
				}
				if chunk.Done {
					doneSent = true
				}
			})
			if err != nil {
				t.Fatalf("filtered finish must be terminal (nil stream error), got %v", err)
			}
			if doneSent {
				t.Error("filtered finish must not emit a Done chunk")
			}
			if gotErr == nil {
				t.Fatal("filtered finish must emit an Error chunk")
			}
			if !strings.Contains(gotErr.Error(), reason) {
				t.Errorf("error chunk should name the finish reason %q, got %v", reason, gotErr)
			}
		})
	}
}

func TestParseGeminiSSE_WithUsageMetadata(t *testing.T) {
	// STOP with usage metadata → tokensIn/Out in Done chunk.
	input := "data: {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"ok\"}]}, \"finishReason\": \"STOP\"}], \"usageMetadata\": {\"promptTokenCount\": 10, \"candidatesTokenCount\": 4}}\n\n"

	var tokensIn, tokensOut int
	err := parseGeminiSSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			tokensIn = chunk.TokensIn
			tokensOut = chunk.TokensOut
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokensIn != 10 {
		t.Errorf("expected TokensIn=10, got %d", tokensIn)
	}
	if tokensOut != 4 {
		t.Errorf("expected TokensOut=4, got %d", tokensOut)
	}
}
