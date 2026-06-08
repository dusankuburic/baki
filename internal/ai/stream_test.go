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
