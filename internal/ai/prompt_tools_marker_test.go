package ai

import (
	"strings"
	"testing"
)

// R-tool-loop-3: a <tool_call> cut at the output cap matched no block, so its raw JSON reached the answer.
func TestStripPromptToolCalls_RemovesAnUnterminatedTail(t *testing.T) {
	text := "Let me look.\n<tool_call>\n{\"name\":\"search_flow\""
	if got := StripPromptToolCalls(text); got != "Let me look.\n" {
		t.Errorf("StripPromptToolCalls = %q, want the text before the unterminated marker", got)
	}
	// A complete block before the unterminated one goes too, and the text between them stays.
	text = "A\n<tool_call>{\"name\":\"get_block\",\"input\":{}}</tool_call>\nB\n<tool_call>{\"name\""
	if got := StripPromptToolCalls(text); got != "A\n\nB\n" {
		t.Errorf("StripPromptToolCalls = %q", got)
	}
}

func TestHasUnterminatedToolCall(t *testing.T) {
	for text, want := range map[string]bool{
		"plain answer": false,
		"<tool_call>{\"name\":\"get_block\"}</tool_call>":                   false,
		"<tool_call>{\"name\":\"get_block\"}</tool_call> then <tool_call>{": true,
		"cut here <tool_call>": true,
	} {
		if got := HasUnterminatedToolCall(text); got != want {
			t.Errorf("HasUnterminatedToolCall(%q) = %v, want %v", text, got, want)
		}
	}
}

// toolEntry is the block of the marker instructions that describes one tool: from its "- name:" line to the next.
func toolEntry(t *testing.T, instr, name string) string {
	t.Helper()
	start := strings.Index(instr, "- "+name+":")
	if start < 0 {
		t.Fatalf("no entry for %s in:\n%s", name, instr)
	}
	rest := instr[start+2:]
	if end := strings.Index(rest, "\n- "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// R-tool-loop-15: the instructions listed names and descriptions only, so a model without native tools had to
// guess every argument name the descriptions do not happen to mention.
func TestToolPromptInstructions_RenderEachToolsInput(t *testing.T) {
	instr := ToolPromptInstructionsFor(nil)
	for name, wants := range map[string][]string{
		"get_subflow_blocks":   {"subflowId"},
		"apply_fixes":          {"targets", "blockId", "ruleId"},
		"get_variable_lineage": {"name"},
		"search_flow":          {"query", "limit"},
		"get_block":            {"blockId"},
	} {
		e := toolEntry(t, instr, name)
		input := e[strings.Index(e, "\n"):] // past the description line
		for _, want := range wants {
			if !strings.Contains(input, want) {
				t.Errorf("%s input does not name %q:\n%s", name, want, e)
			}
		}
	}
	// Required and optional arguments are told apart: search_flow's limit is optional, its query is not.
	if e := toolEntry(t, instr, "search_flow"); !strings.Contains(e, "query: string") || !strings.Contains(e, "limit?: integer") {
		t.Errorf("search_flow entry does not mark required and optional arguments:\n%s", e)
	}
	if !strings.Contains(instr, `{"name": "get_block", "input": {"blockId":`) {
		t.Errorf("the instructions carry no worked example call:\n%s", instr)
	}
}

// The loop runs every block a response carries; the instructions must say so, not "at most one".
func TestToolPromptInstructions_AgreeWithTheLoopOnSeveralCalls(t *testing.T) {
	instr := ToolPromptInstructionsFor(nil)
	if strings.Contains(instr, "at most one tool_call") {
		t.Errorf("the instructions forbid several calls, which the loop runs:\n%s", instr)
	}
	if !strings.Contains(instr, "several tool_call blocks") {
		t.Errorf("the instructions do not say several calls run together:\n%s", instr)
	}
	calls := ParsePromptToolCalls("<tool_call>{\"name\":\"a\"}</tool_call><tool_call>{\"name\":\"b\"}</tool_call>")
	if len(calls) != 2 {
		t.Errorf("the parser keeps %d of two blocks", len(calls))
	}
}

// The instructions ride every marker-mode request's system prompt: they stay bounded.
func TestToolPromptInstructions_StayBounded(t *testing.T) {
	const maxBytes = 4096
	if n := len(ToolPromptInstructionsFor(nil)); n > maxBytes {
		t.Errorf("the marker instructions are %d bytes, over the %d-byte bound", n, maxBytes)
	}
}

// Review of step 3: the cut applied to any unclosed "<tool_call>", so an answer that merely mentions the marker
// in prose lost everything after it, and a marker cut inside its own name leaked.
func TestStripPromptToolCalls_KeepsAProseMention(t *testing.T) {
	for _, text := range []string{
		"Models emit `<tool_call>` blocks when they need data; then the loop runs them.",
		"Write <tool_call> and then the JSON.",
		"Ends with <t",
	} {
		if got := StripPromptToolCalls(text); got != text {
			t.Errorf("StripPromptToolCalls(%q) = %q, want the text unchanged", text, got)
		}
		if HasUnterminatedToolCall(text) {
			t.Errorf("HasUnterminatedToolCall(%q) = true for a prose mention", text)
		}
	}
}

func TestStripPromptToolCalls_CutsACallOrAMarkerCutShort(t *testing.T) {
	for text, want := range map[string]string{
		"Let me look.\n<tool_call>\n  {\"name\":": "Let me look.\n",
		"Let me look.\n<tool_call>":               "Let me look.\n",
		"Let me look.\n<tool_call>  \n":           "Let me look.\n",
		"Let me look.\n<tool_ca":                  "Let me look.\n",
		"Let me look.\n<tool":                     "Let me look.\n",
	} {
		if got := StripPromptToolCalls(text); got != want {
			t.Errorf("StripPromptToolCalls(%q) = %q, want %q", text, got, want)
		}
		if !HasUnterminatedToolCall(text) {
			t.Errorf("HasUnterminatedToolCall(%q) = false", text)
		}
	}
}
