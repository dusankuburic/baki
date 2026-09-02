package service

import (
	"context"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// promptLoopProvider records each request in full (messages + system prompt)
// and scripts marker-format turns, as GitHub Copilot produces them.
type promptLoopProvider struct {
	testutil.FakeProvider
	mu   sync.Mutex
	reqs []ai.Request
}

func (p *promptLoopProvider) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	return p.FakeProvider.Stream(ctx, req, onChunk)
}

// TestRunPromptToolLoop_FeedbackFormatIsSchemaValid pins A3 + the truncation
// pin for the prompt-based tool loop (Copilot):
//
//  1. Tool results feed back as a USER message carrying <tool_result> markers.
//     The pre-fix code appended a bare role:"tool" message — schema-invalid on
//     OpenAI-compatible wires (tool messages must answer an assistant
//     tool_calls turn this protocol never emits), so Copilot 400-rejected the
//     second request of every tool conversation.
//  2. The tool protocol lives in the request's SystemPrompt, not an injected
//     leading system message — the injected variant took the truncation pin
//     (Messages[:1]), so under context overflow the junction repair dropped
//     the USER'S QUESTION while pinning tool-instruction boilerplate.
func TestRunPromptToolLoop_FeedbackFormatIsSchemaValid(t *testing.T) {
	prov := &promptLoopProvider{FakeProvider: testutil.FakeProvider{Tools: false, Turns: []testutil.FakeTurn{
		{Text: "Let me check.\n<tool_call>\n{\"name\": \"search_flow\", \"input\": {\"query\": \"xav\"}}\n</tool_call>", TokensIn: 10, TokensOut: 5},
		{Text: "Found it — the Xavier block calls the API.", TokensIn: 8, TokensOut: 3},
	}}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	svc.runPromptToolLoop(context.Background(), prov, ai.Request{
		SystemPrompt: "You are an assistant.",
		Messages:     []ai.Message{{Role: "user", Content: "where is the API call?"}},
	}, "user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("loop did not finish: done=%v errMsg=%q", ctl.done, ctl.errMsg)
	}
	// The cleaned preamble + the final answer both stream; the raw marker JSON
	// must never reach the client.
	if !strings.Contains(ctl.buffer.String(), "Let me check.") || !strings.Contains(ctl.buffer.String(), "Found it") {
		t.Fatalf("buffer missing preamble/final answer: %q", ctl.buffer.String())
	}
	if strings.Contains(ctl.buffer.String(), "<tool_call>") || strings.Contains(ctl.buffer.String(), `"search_flow"`) {
		t.Fatalf("raw marker leaked to the client buffer: %q", ctl.buffer.String())
	}
	// Preamble chunk streams first, then the tool events, then the final text.
	if got := strings.Join(*evs, ","); got != "chunk,tool,tool_result,chunk,done" {
		t.Errorf("expected chunk,tool,tool_result,chunk,done events, got %q", got)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.reqs) != 2 {
		t.Fatalf("want 2 model turns, got %d", len(prov.reqs))
	}

	// (2) The protocol rides the SystemPrompt; NO synthetic system message.
	second := prov.reqs[1]
	if !strings.Contains(second.SystemPrompt, "<tool_call>") {
		t.Errorf("tool protocol missing from the system prompt of turn 2: %q", second.SystemPrompt)
	}
	if !strings.Contains(second.SystemPrompt, "You are an assistant.") {
		t.Errorf("caller system prompt lost: %q", second.SystemPrompt)
	}
	for i, m := range second.Messages {
		if m.Role == "system" {
			t.Errorf("message %d is a synthetic system message — truncation pin regression", i)
		}
	}
	// The pinned head is the user's actual question.
	if len(second.Messages) == 0 || second.Messages[0].Role != "user" || !strings.Contains(second.Messages[0].Content, "where is the API call?") {
		t.Errorf("first message must stay the user's question, got %+v", second.Messages[:1])
	}

	// (1) The feedback message is a user-role <tool_result>, never role:"tool".
	var feedback *ai.Message
	for i := range second.Messages {
		if second.Messages[i].Role == "tool" {
			t.Fatalf("bare role:%q message at index %d — schema-invalid on OpenAI-compatible wires", second.Messages[i].Role, i)
		}
		if second.Messages[i].Role == "user" && strings.Contains(second.Messages[i].Content, "<tool_result") {
			m := second.Messages[i]
			feedback = &m
		}
	}
	if feedback == nil {
		t.Fatalf("no <tool_result> user message in turn 2: %+v", second.Messages)
	}
	if !strings.Contains(feedback.Content, `<tool_result name="search_flow">`) {
		t.Errorf("feedback marker malformed: %q", feedback.Content)
	}
	if !strings.Contains(feedback.Content, "match(es)") && !strings.Contains(feedback.Content, "No blocks matched") {
		t.Errorf("feedback does not carry the tool result: %q", feedback.Content)
	}
	// The assistant's marker turn is preserved verbatim before the feedback.
	if len(second.Messages) < 3 || second.Messages[len(second.Messages)-2].Role != "assistant" {
		t.Errorf("assistant marker turn missing before feedback: %+v", second.Messages)
	}
}
