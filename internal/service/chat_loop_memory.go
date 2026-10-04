package service

import (
	"encoding/json"
	"strings"
	"sync"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/metrics"
)

// fixDeclinedMarker is the stable prefix of the model-facing result for a user-declined
// fix (chatFixApplier); declared here so the loops' decline memory and the applier's
// message cannot drift apart.
const fixDeclinedMarker = "the user DECLINED"

// loopToolMemory gives one tool-loop run repetition detection: a model can re-emit the
// IDENTICAL call (name + raw args) every iteration — re-billing a growing prompt with
// zero progress until the iteration cap; the second identical call returns the cached
// result with a "change tactic" note. Fix tools additionally remember user DECLINES and
// refuse to re-request the same approval (the prompt-level "do not call apply_fix again"
// note was the only enforcement before).
type loopToolMemory struct {
	mu       sync.Mutex
	results  map[string]string // signature -> first result
	declined map[string]bool   // fix-tool signature -> user declined
}

func newLoopToolMemory() *loopToolMemory {
	return &loopToolMemory{results: map[string]string{}, declined: map[string]bool{}}
}

func isFixTool(name string) bool { return name == "apply_fix" || name == "apply_fixes" }

// exec runs one tool call through the repetition guard. Signature is the tool name +
// raw input JSON: identical args means the model gained nothing since the last identical
// call (docs are content-addressed per turn; only applies mutate the doc, and those
// re-resolve to "no finding" naturally once the re-parse changes IDs).
func (m *loopToolMemory) exec(name string, input json.RawMessage, tctx *ai.ToolContext) string {
	sig := name + "\x00" + string(input)

	m.mu.Lock()
	if isFixTool(name) && m.declined[sig] {
		m.mu.Unlock()
		const out = "error: the user already DECLINED this exact fix — nothing was changed. Do not propose it again unless they explicitly ask; explain the change or move on."
		metrics.RecordChatToolResult(metricToolLabel(name), false)
		return out
	}
	cached, seen := m.results[sig]
	m.mu.Unlock()

	if seen {
		metrics.RecordChatToolResult(metricToolLabel(name), !strings.HasPrefix(cached, "error:"))
		return cached + "\n(note: this exact call was already answered above — change tactic or give your final answer)"
	}

	result := ai.ExecuteTool(name, input, *tctx)
	metrics.RecordChatToolResult(metricToolLabel(name), !strings.HasPrefix(result, "error:"))

	m.mu.Lock()
	m.results[sig] = result
	if isFixTool(name) && strings.Contains(result, fixDeclinedMarker) {
		m.declined[sig] = true
	}
	m.mu.Unlock()
	return result
}

// knownToolNames is the closed label set for pad_chat_tool_results_total{tool}: the
// label used to be the model-emitted tool name verbatim, so a hallucinated name minted a
// permanent per-request time series — the cardinality bomb this codebase has already had
// to fix twice (route labels, invite tokens).
var knownToolNames = func() map[string]bool {
	m := map[string]bool{}
	for _, d := range ai.ToolDefinitions() {
		m[d.Name] = true
	}
	return m
}()

// metricToolLabel maps a tool name to its metric label: itself when it is a
// registered tool, "other" for anything the model made up.
func metricToolLabel(name string) string {
	if knownToolNames[name] {
		return name
	}
	return "other"
}
