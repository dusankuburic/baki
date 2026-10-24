package ai

import (
	"fmt"
	"regexp"
	"strings"
)

// The data frames (ADR 0036). Everything the flow's author or the organisation's documents control reaches the
// model inside a named frame — the flow context, the knowledge base's guidelines, a marker-mode tool result —
// and SystemPromptDefault says that what is inside a frame is material to analyse, never instructions. A frame
// only holds if nothing inside it can close it or open another, so the content is escaped: the '<' of any
// frame tag (opening or closing, any case) and of the prompt loop's tool_call marker becomes "&lt;", which the
// model reads as text. Nothing else is touched: PAD's comparisons (<>), arrows (=>) and any other markup stay as
// written. Native tool results travel in the provider's tool-result channel, which is a frame of its own; the
// rule in the system prompt covers them.
const (
	FrameFlowContext = "flow_context"
	FrameGuidelines  = "org_guidelines"
	FrameToolResult  = "tool_result"
	// frameToolCall is the marker the prompt loop parses from the MODEL's text (prompt_tools.go); inside a frame
	// it is text like any other and must not survive as a marker.
	frameToolCall = "tool_call"
)

var frameTagRe = regexp.MustCompile(`(?i)<\s*(/?)\s*(` + FrameFlowContext + `|` + FrameGuidelines + `|` + FrameToolResult + `|` + frameToolCall + `)\b`)

// EscapeFrameText neutralises every frame tag in s so s can sit inside a frame without closing it.
func EscapeFrameText(s string) string {
	if !strings.ContainsRune(s, '<') {
		return s
	}
	return frameTagRe.ReplaceAllString(s, "&lt;$1$2")
}

// Frame wraps content in <tag>…</tag>, escaped.
func Frame(tag, content string) string {
	return "<" + tag + ">\n" + EscapeFrameText(content) + "\n</" + tag + ">"
}

// ToolResultFrame is the frame a marker-mode tool result is fed back in: the tool's name on the tag, the result
// escaped. The prompt loop's instructions promise this shape (ToolPromptInstructionsFor).
func ToolResultFrame(name, result string) string {
	return fmt.Sprintf("<%s name=%q>\n%s\n</%s>", FrameToolResult, name, EscapeFrameText(result), FrameToolResult)
}
