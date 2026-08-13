package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParsePromptToolCalls_ExtractsValidBlocks(t *testing.T) {
	text := `Let me search for HTTP usage.
<tool_call>
{"name": "search_flow", "input": {"query": "HTTP"}}
</tool_call>
`
	calls := ParsePromptToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "search_flow" {
		t.Errorf("name = %q, want search_flow", calls[0].Name)
	}
	var input map[string]string
	if err := json.Unmarshal(calls[0].Input, &input); err != nil {
		t.Fatalf("input not valid JSON: %v", err)
	}
	if input["query"] != "HTTP" {
		t.Errorf("input.query = %q, want HTTP", input["query"])
	}
}

func TestParsePromptToolCalls_MultipleBlocksAndNoInput(t *testing.T) {
	text := `<tool_call>{"name": "list_findings"}</tool_call>
<tool_call>{"name": "get_block", "input": {"blockId": "b9"}}</tool_call>`
	calls := ParsePromptToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	// A missing input should default to {} so ExecuteTool gets valid JSON.
	if string(calls[0].Input) != "{}" {
		t.Errorf("missing input should default to {}, got %s", calls[0].Input)
	}
}

func TestParsePromptToolCalls_NoneWhenAbsent(t *testing.T) {
	if calls := ParsePromptToolCalls("just a normal answer with no markers"); calls != nil {
		t.Errorf("expected nil, got %v", calls)
	}
}

func TestParsePromptToolCalls_SkipsMalformed(t *testing.T) {
	// A broken JSON payload is skipped, not fatal — the model recovers next turn.
	text := "<tool_call>not json</tool_call>\n<tool_call>{\"name\":\"ok\"}</tool_call>"
	calls := ParsePromptToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 valid call (malformed skipped), got %d", len(calls))
	}
	if calls[0].Name != "ok" {
		t.Errorf("name = %q", calls[0].Name)
	}
}

func TestStripPromptToolCalls_RemovesBlocks(t *testing.T) {
	text := `Preamble.
<tool_call>
{"name": "search_flow", "input": {}}
</tool_call>
Trailer.`
	stripped := StripPromptToolCalls(text)
	if strings.Contains(stripped, "tool_call") {
		t.Errorf("marker not stripped: %q", stripped)
	}
	if !strings.Contains(stripped, "Preamble.") || !strings.Contains(stripped, "Trailer.") {
		t.Errorf("non-marker text lost: %q", stripped)
	}
}

func TestToolPromptInstructions_ListsAllTools(t *testing.T) {
	instr := ToolPromptInstructions()
	// Every registered tool name should appear in the instructions.
	for _, td := range ToolDefinitions() {
		if !strings.Contains(instr, td.Name) {
			t.Errorf("instructions missing tool %q", td.Name)
		}
	}
	if !strings.Contains(instr, "<tool_call>") {
		t.Error("instructions don't mention the <tool_call> marker format")
	}
}
