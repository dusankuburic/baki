package service

import (
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// TestTruncateForContextWindow_ToolOverhead pins R4b: the window estimate
// counts the tool schemas riding every request AND each assistant tool_use's
// argument JSON — a content-only estimate undercounted a tool loop by the
// schema (~1000 tokens) plus accumulated args, letting requests pass the
// guard and 400 at the provider.
func TestTruncateForContextWindow_ToolOverhead(t *testing.T) {
	// FakeProvider.EstimateTokens = len/4.
	mkReq := func() *ai.Request {
		return &ai.Request{
			Messages: []ai.Message{
				{Role: "user", Content: stringRepeat("q", 400)}, // 100 tokens
				{Role: "assistant", Content: "", ToolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: []byte(stringRepeat(`{"query":"x"}`, 150))}}}, // args ~300 tokens
				{Role: "tool", Content: stringRepeat("r", 1200)}, // 300 tokens
			},
		}
	}
	provider := &testutil.FakeProvider{}
	const ctxLimit = 5000

	// Control: without tools the conversation fits untouched.
	req := mkReq()
	if err := truncateForContextWindow(provider, req, ctxLimit); err != nil {
		t.Fatalf("no-tools case should fit: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Errorf("no-tools case must not truncate, got %d messages", len(req.Messages))
	}

	// With a tool schema attached (~500 tokens), the same conversation must
	// now overflow and shed the assistant+tool turn (the junction repair
	// drops the orphaned tool result with its assistant). The schema is sized
	// so the pinned user turn + schemas still fit — a request whose TOOLS
	// alone overflow the window fails outright (different branch).
	req = mkReq()
	req.Tools = []ai.ToolDefinition{{Name: "search_flow", InputSchema: []byte(stringRepeat("s", 2000))}}
	if err := truncateForContextWindow(provider, req, ctxLimit); err != nil {
		t.Fatalf("with-tools case should truncate, not fail: %v", err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Errorf("with-tools case must shed history down to the pinned user turn, got %+v", req.Messages)
	}
}

func stringRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
