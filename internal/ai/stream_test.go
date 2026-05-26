package ai

import (
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
