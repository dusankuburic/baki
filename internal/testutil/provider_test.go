package testutil

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
)

// recordingTB keeps the guard's reports instead of failing the test that checks them.
type recordingTB struct {
	testing.TB
	reports []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.reports = append(r.reports, fmt.Sprintf(format, args...))
}

// TestFakeProvider_RequireScrubbedReportsEveryUnscrubbedString: the guard names each string of a request that
// the scrubber would still change, by its path, and says nothing about a request whose strings are already
// their own scrub — the egress rule the chat service tests run under.
func TestFakeProvider_RequireScrubbedReportsEveryUnscrubbedString(t *testing.T) {
	rec := &recordingTB{TB: t}
	p := &FakeProvider{RequireScrubbed: rec}
	leaky := ai.Request{
		SystemPrompt: "You help with flows.",
		Messages: []ai.Message{
			{Role: "user", Content: "it fails with password: hunter2secret on the desktop"},
			{Role: "assistant", ToolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: []byte(`{"query":"Authorization: Bearer abcdefghijklmnop"}`)}}},
		},
		Tools: []ai.ToolDefinition{{Name: "search_flow", Description: "Search the flow.", InputSchema: []byte(`{"type":"object"}`)}},
	}
	if err := p.Stream(context.Background(), leaky, func(ai.Chunk) {}); err != nil {
		t.Fatal(err)
	}
	if len(rec.reports) != 2 {
		t.Fatalf("reports = %q, want one per unscrubbed string", rec.reports)
	}
	for i, want := range []string{"messages[0].content (user)", "messages[1].toolCalls[0].input (search_flow)"} {
		if !strings.Contains(rec.reports[i], want) || !strings.HasPrefix(rec.reports[i], "request 1 ") {
			t.Errorf("report %d = %q, want the path %q of request 1", i, rec.reports[i], want)
		}
	}

	clean := leaky
	clean.Messages = []ai.Message{{Role: "user", Content: "it fails with password: [REDACTED] on the desktop"}}
	if err := p.Stream(context.Background(), clean, func(ai.Chunk) {}); err != nil {
		t.Fatal(err)
	}
	if len(rec.reports) != 2 {
		t.Errorf("a request of scrubbed strings was reported: %q", rec.reports[2:])
	}
}
