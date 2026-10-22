package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// serveGeminiError points the Gemini provider at a fake API answering every
// request with the given status + JSON error body.
func serveGeminiError(t *testing.T, status int, message string) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error": {"code": ` + strconv.Itoa(status) + `, "message": "` + message + `", "status": "INVALID_ARGUMENT"}}`))
	}))
	prev := geminiBaseHost
	geminiBaseHost = srv.URL
	t.Cleanup(func() {
		srv.Close()
		geminiBaseHost = prev
	})
	return srv.Client()
}

// geminiContextLimitMsg is Gemini's actual wording for an oversized prompt.
// It matches none of the OpenAI/Claude context-limit phrasings, which is why
// the Gemini paths previously surfaced a raw 400 instead of ErrContextLimit
// (bypassing the chat service's friendly "conversation too long" mapping).
const geminiContextLimitMsg = "The input token count (1234567) exceeds the maximum number of tokens allowed (1048576)."

func TestGeminiChat_ContextLimit400_MapsToErrContextLimit(t *testing.T) {
	client := serveGeminiError(t, http.StatusBadRequest, geminiContextLimitMsg)
	p := &GeminiProvider{apiKey: "key", client: client}

	_, err := p.Chat(context.Background(), Request{Model: "gemini-2.5-pro"})
	if !errors.Is(err, ErrContextLimit) {
		t.Errorf("want ErrContextLimit for oversized-prompt 400, got %v", err)
	}
}

func TestGeminiStream_ContextLimit400_MapsToErrContextLimit(t *testing.T) {
	client := serveGeminiError(t, http.StatusBadRequest, geminiContextLimitMsg)
	p := &GeminiProvider{apiKey: "key", client: client}

	err := p.Stream(context.Background(), Request{Model: "gemini-2.5-pro"}, func(Chunk) {})
	if !errors.Is(err, ErrContextLimit) {
		t.Errorf("want ErrContextLimit for oversized-prompt 400, got %v", err)
	}
}

func TestGeminiChat_NonContext400_NotErrContextLimit(t *testing.T) {
	client := serveGeminiError(t, http.StatusBadRequest, "Unable to submit request because it has an invalid function call")
	p := &GeminiProvider{apiKey: "key", client: client}

	_, err := p.Chat(context.Background(), Request{Model: "gemini-2.5-pro"})
	if err == nil {
		t.Fatal("want error for 400, got nil")
	}
	if errors.Is(err, ErrContextLimit) {
		t.Errorf("unrelated 400 must not map to ErrContextLimit, got %v", err)
	}
	if !strings.Contains(err.Error(), "invalid function call") {
		t.Errorf("original message should be preserved, got %v", err)
	}
}

// TestGeminiToolCallIDs_UniqueAcrossStreams_AttributeCorrectly reproduces the
// tool-loop mis-attribution bug: parse two separate Gemini streams (iterations
// 1 and 2 of an agent loop), each requesting one tool call. With per-stream
// "call_%d" IDs both calls were call_0, so toGeminiContents' history-wide
// callNames map resolved iteration 1's tool result to iteration 2's function
// name. With process-unique IDs each result keeps its own name.
func TestGeminiToolCallIDs_UniqueAcrossStreams_AttributeCorrectly(t *testing.T) {
	stream := func(fn string) []ToolCall {
		var calls []ToolCall
		input := "data: {\"candidates\": [{\"content\": {\"parts\": [{\"functionCall\": {\"name\": \"" + fn + "\", \"args\": {}}}]}, \"finishReason\": \"STOP\"}]}\n\n"
		if err := parseGeminiSSE(strings.NewReader(input), func(c Chunk) {
			if c.Done {
				calls = c.ToolCalls
			}
		}); err != nil {
			t.Fatalf("parse stream for %s: %v", fn, err)
		}
		if len(calls) != 1 {
			t.Fatalf("stream for %s: want 1 tool call, got %d", fn, len(calls))
		}
		return calls
	}

	first := stream("search_flow")
	second := stream("get_block")
	if first[0].ID == second[0].ID {
		t.Fatalf("tool-call IDs collided across streams: both %q", first[0].ID)
	}

	// Rebuild the loop's message history and serialize it the way the next
	// request would: each tool result must carry ITS function's name.
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: first},
		{Role: "tool", Content: "result-1", ToolCallID: first[0].ID},
		{Role: "assistant", ToolCalls: second},
		{Role: "tool", Content: "result-2", ToolCallID: second[0].ID},
	}
	var gotNames []string
	for _, c := range toGeminiContents(msgs) {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				gotNames = append(gotNames, p.FunctionResponse.Name)
			}
		}
	}
	want := []string{"search_flow", "get_block"}
	if len(gotNames) != len(want) {
		t.Fatalf("want %d function responses, got %d (%v)", len(want), len(gotNames), gotNames)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Errorf("function response %d: want name %q, got %q (mis-attributed tool result)", i, want[i], gotNames[i])
		}
	}
}

// TestToGeminiContents_ParallelToolResultsCoalesced (R-tool-loop-12): a turn with two function calls is
// answered by ONE user content carrying both function responses, in call order — Gemini rejects a request
// whose function-response parts do not line up with the preceding model turn's function-call parts.
func TestToGeminiContents_ParallelToolResultsCoalesced(t *testing.T) {
	calls := []ToolCall{
		{ID: "call_a", Name: "search_flow", Input: schema(`{"q":"x"}`)},
		{ID: "call_b", Name: "get_block", Input: schema(`{"id":"1"}`)},
	}
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: calls},
		{Role: "tool", Content: "result-a", ToolCallID: "call_a"},
		{Role: "tool", Content: "result-b", ToolCallID: "call_b"},
	}
	contents := toGeminiContents(msgs)
	if len(contents) != 3 {
		roles := make([]string, len(contents))
		for i, c := range contents {
			roles[i] = c.Role
		}
		t.Fatalf("want 3 contents (user, model, user), got %d: %v", len(contents), roles)
	}
	model := contents[1]
	if model.Role != "model" || len(model.Parts) != 2 || model.Parts[0].FunctionCall == nil || model.Parts[1].FunctionCall == nil {
		t.Fatalf("content 1 = role %q with %d parts, want the model turn with two functionCall parts", model.Role, len(model.Parts))
	}
	user := contents[2]
	if user.Role != "user" || len(user.Parts) != 2 {
		t.Fatalf("content 2 = role %q with %d parts, want one user content with two functionResponse parts", user.Role, len(user.Parts))
	}
	for i, want := range []string{"search_flow", "get_block"} {
		fr := user.Parts[i].FunctionResponse
		if fr == nil || fr.Name != want {
			t.Errorf("functionResponse %d = %+v, want name %q (call order)", i, fr, want)
		}
	}
}

// TestToGeminiContents_UnknownCallIDKeepsName (R-tool-loop-12): a tool result whose call is missing from
// the history (trimmed context) is still sent under a non-empty name — Gemini refuses a functionResponse
// without one.
func TestToGeminiContents_UnknownCallIDKeepsName(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "tool", Content: "result", ToolCallID: "call_orphan"},
	}
	var names []string
	for _, c := range toGeminiContents(msgs) {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				names = append(names, p.FunctionResponse.Name)
			}
		}
	}
	if len(names) != 1 || names[0] == "" {
		t.Fatalf("functionResponse names = %q, want exactly one non-empty name for the orphaned result", names)
	}
}
