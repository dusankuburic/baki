package service

import (
	"encoding/json"
	"strings"
	"sync"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/metrics"
)

// fixDeclinedMarker is the stable prefix of the model-facing result for a
// user-declined fix (chatFixApplier). Declared here so the loops' decline
// memory and the applier's message cannot drift apart.
const fixDeclinedMarker = "the user DECLINED"

// loopToolMemory gives one tool-loop run repetition detection: a model can
// re-emit the IDENTICAL call (name + raw args) every iteration — re-billing a
// growing prompt with zero progress until the iteration cap. The second
// identical call returns the cached result with a "change tactic" note; fix
// tools additionally remember user DECLINES and refuse to re-request the same
// approval (the prompt-level "do not call apply_fix again" note was the only
// enforcement before).
type loopToolMemory struct {
	mu       sync.Mutex
	results  map[string]string // signature -> first result
	declined map[string]bool   // fix-tool signature -> user declined
}

func newLoopToolMemory() *loopToolMemory {
	return &loopToolMemory{results: map[string]string{}, declined: map[string]bool{}}
}

func isFixTool(name string) bool { return name == "apply_fix" || name == "apply_fixes" }

// exec runs one tool call through the repetition guard. Signature is the tool
// name + raw input JSON: identical args means the model gained nothing since
// the last identical call (docs are content-addressed per turn; only applies
// mutate the doc, and those re-resolve to "no finding" naturally once the
// re-parse changes IDs).
func (m *loopToolMemory) exec(name string, input json.RawMessage, tctx *ai.ToolContext) string {
	sig := name + "\x00" + string(input)

	m.mu.Lock()
	if isFixTool(name) && m.declined[sig] {
		m.mu.Unlock()
		const out = "error: the user already DECLINED this exact fix — nothing was changed. Do not propose it again unless they explicitly ask; explain the change or move on."
		metrics.RecordChatToolResult(name, false)
		return out
	}
	cached, seen := m.results[sig]
	m.mu.Unlock()

	if seen {
		metrics.RecordChatToolResult(name, !strings.HasPrefix(cached, "error:"))
		return cached + "\n(note: this exact call was already answered above — change tactic or give your final answer)"
	}

	result := ai.ExecuteTool(name, input, *tctx)
	metrics.RecordChatToolResult(name, !strings.HasPrefix(result, "error:"))

	m.mu.Lock()
	m.results[sig] = result
	if isFixTool(name) && strings.Contains(result, fixDeclinedMarker) {
		m.declined[sig] = true
	}
	m.mu.Unlock()
	return result
}
