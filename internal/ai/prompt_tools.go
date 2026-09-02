package ai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// PromptToolCall is a tool invocation parsed from a model's free-form text
// (the prompt-based fallback for providers without native function-calling,
// e.g. GitHub Copilot). It mirrors the native ToolCall's intent: a name + the
// raw JSON input the handler will decode.
type PromptToolCall struct {
	Name  string
	Input json.RawMessage
}

// toolCallRe matches a <tool_call>{...}</tool_call> block. The (?s) flag lets
// the inner JSON span newlines; the non-greedy .*? keeps one block from
// swallowing a following block. The model is instructed to emit exactly this
// shape when it wants to call a tool.
var toolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// ToolPromptInstructions returns the system-prompt text that teaches a
// function-calling-less model how to invoke the grounding tools. It lists each
// tool and the exact marker format the parser expects. Append this to the
// system prompt when running the prompt-based tool loop (runPromptToolLoop).
func ToolPromptInstructions() string {
	var b strings.Builder
	b.WriteString("You have access to tools to inspect the currently open flow (and, for apply_fix, to request a user-approved fix). ")
	b.WriteString("To call a tool, output exactly one block on its own in this format:\n")
	b.WriteString("<tool_call>\n{\"name\": \"<tool_name>\", \"input\": {<args as JSON>}}\n</tool_call>\n")
	b.WriteString("After you emit a tool_call block, the tool runs and its result comes back to you as the next user message, in the form:\n")
	b.WriteString("<tool_result name=\"<tool_name>\">\n<result text>\n</tool_result>\n")
	b.WriteString("When you have enough information for your final answer, respond normally WITHOUT any tool_call block.\n\n")
	b.WriteString("Available tools:\n")
	for _, td := range ToolDefinitions() {
		fmt.Fprintf(&b, "- %s: %s\n", td.Name, td.Description)
	}
	b.WriteString("\nEmit at most one tool_call per response; wait for its result before calling the next.\n")
	return b.String()
}

// ParsePromptToolCalls extracts every <tool_call> block from the model's text,
// decoding the JSON payload. Malformed blocks are skipped (the model can
// recover on the next turn) rather than aborting the loop.
func ParsePromptToolCalls(text string) []PromptToolCall {
	matches := toolCallRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	calls := make([]PromptToolCall, 0, len(matches))
	for _, m := range matches {
		var parsed struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal([]byte(m[1]), &parsed); err != nil || parsed.Name == "" {
			continue
		}
		if len(parsed.Input) == 0 {
			parsed.Input = json.RawMessage("{}")
		}
		calls = append(calls, PromptToolCall{Name: parsed.Name, Input: parsed.Input})
	}
	return calls
}

// StripPromptToolCalls removes every <tool_call> block from the text so the raw
// tool JSON isn't shown to the user — only the model's natural-language preamble
// and final answer are emitted to the client.
func StripPromptToolCalls(text string) string {
	return toolCallRe.ReplaceAllString(text, "")
}
