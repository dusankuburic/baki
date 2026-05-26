package ai

import (
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

func TestParseClaudeSSE_EOF_WithoutDone_AutoSendsDone(t *testing.T) {
	// Stream ends without any message_delta stop_reason or [DONE] → auto-send Done.
	input := "event: content_block_delta\n" +
		"data: {\"delta\": {\"type\": \"text_delta\", \"text\": \"partial\"}}\n\n"

	var doneSent bool
	err := parseClaudeSSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			doneSent = true
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !doneSent {
		t.Error("expected auto-done when stream ends without explicit stop signal")
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

func TestParseOpenAISSE_EOF_WithoutDone_AutoSendsDone(t *testing.T) {
	// Stream ends without [DONE] → auto-send Done.
	input := "data: {\"choices\": [{\"delta\": {\"content\": \"hi\"}}]}\n\n"

	var doneSent bool
	err := parseOpenAISSE(strings.NewReader(input), func(chunk Chunk) {
		if chunk.Done {
			doneSent = true
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !doneSent {
		t.Error("expected auto-done when stream ends without [DONE]")
	}
}

// ---- parseGeminiSSE: EOF without doneSent → auto-done ---------------------

func TestParseGeminiSSE_EOF_WithoutDone_AutoSendsDone(t *testing.T) {
	// Stream ends without STOP finish reason → auto-send Done.
	input := "data: {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"partial\"}]}}]}\n\n"

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
		t.Error("expected auto-done when stream ends without STOP reason")
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
