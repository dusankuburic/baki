package ai

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// ---- parseClaudeSSE: uncovered branches ------------------------------------

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

// ---- parseOpenAISSE: uncovered branches ------------------------------------

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

// ---- parseGeminiSSE: EOF without doneSent → auto-done ---------------------

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
// the behavior fix that any non-empty FinishReason (MAX_TOKENS, SAFETY,
// RECITATION, OTHER) is treated as a clean stream end, not a truncation.
// Previously only "STOP" was considered terminal, so a token-capped
// response was incorrectly auto-Done'd / now would be wrongly reported as
// truncated.
func TestParseGeminiSSE_NonStopFinishReason_TerminalAndNotTruncation(t *testing.T) {
	cases := []string{"STOP", "MAX_TOKENS", "SAFETY", "RECITATION", "OTHER"}
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
