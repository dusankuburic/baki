package service

import (
	"encoding/json"
	"strings"
	"sync"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-core/models"
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
//
// The cache is valid for one document. An approved fix re-parses the flow and swaps the
// tool context's document (onDocReplaced): from then on every cached read describes a flow
// that no longer exists — the old listing names block IDs the re-parse retired — so the
// cache is dropped. Declines are not: they are kept by finding identity, which survives the
// re-parse, as well as by raw arguments.
type loopToolMemory struct {
	mu       sync.Mutex
	doc      *models.FlowDocument // the document the cached results were computed against
	results  map[string]string    // signature -> first result
	declined map[string]bool      // fix-tool signature, or a finding identity (ai.FixTargetIdentities) -> user declined
}

func newLoopToolMemory() *loopToolMemory {
	return &loopToolMemory{results: map[string]string{}, declined: map[string]bool{}}
}

// cacheable reports whether a result may answer an identical call later: a failure ("error:…") or the
// no-analysis sentinel may not be the answer next time, and pinning it made a retry pointless.
func cacheable(result string) bool {
	return !strings.HasPrefix(result, "error:") && !strings.HasPrefix(result, "analysis not available")
}

// currentDoc is the document the tool context reads, the real one when the scrubbed copy has one.
func currentDoc(tctx *ai.ToolContext) *models.FlowDocument {
	if tctx.RealDoc != nil {
		return tctx.RealDoc
	}
	return tctx.Doc
}

// exec runs one tool call through the repetition guard. Signature is the tool name + raw input JSON:
// identical args on the same document means the model gained nothing since the last identical call.
func (m *loopToolMemory) exec(name string, input json.RawMessage, tctx *ai.ToolContext) string {
	sig := name + "\x00" + string(input)
	var identities []string
	if ai.ToolRequiresApproval(name) {
		identities = ai.FixTargetIdentities(*tctx, name, input)
	}

	m.mu.Lock()
	if doc := currentDoc(tctx); doc != m.doc {
		m.doc = doc
		clear(m.results)
	}
	if ai.ToolRequiresApproval(name) && (m.declined[sig] || m.allDeclined(identities)) {
		m.mu.Unlock()
		const out = "error: the user already DECLINED this exact fix — nothing was changed. Do not propose it again unless they explicitly ask; explain the change or move on."
		recordChatToolResult(metricToolLabel(name), false)
		return out
	}
	cached, seen := m.results[sig]
	m.mu.Unlock()

	if seen {
		recordChatToolResult(metricToolLabel(name), !strings.HasPrefix(cached, "error:"))
		return cached + "\n(note: this exact call was already answered above — change tactic or give your final answer)"
	}

	// A call the session's gate refuses (ExecuteTool answers it) is counted by reason: a model that keeps
	// calling a tool it was not offered is visible without reading transcripts.
	if spec, ok := ai.ToolSpecOf(name); ok {
		if reason := ai.ToolRefusal(spec, auth.ActorFromContext(tctx.Ctx), tctx.Fixes != nil); reason != "" {
			recordChatToolRefused(name, reason)
		}
	}
	result := ai.ExecuteTool(name, input, *tctx)
	recordChatToolResult(metricToolLabel(name), !strings.HasPrefix(result, "error:"))

	m.mu.Lock()
	if cacheable(result) {
		m.results[sig] = result
	}
	if ai.ToolRequiresApproval(name) && strings.Contains(result, fixDeclinedMarker) {
		m.declined[sig] = true
		for _, id := range identities {
			m.declined[id] = true
		}
	}
	m.mu.Unlock()
	return result
}

// allDeclined reports whether every finding a fix call targets was declined before; m.mu held. A batch that
// adds a finding the user has not seen asks again.
func (m *loopToolMemory) allDeclined(identities []string) bool {
	if len(identities) == 0 {
		return false
	}
	for _, id := range identities {
		if !m.declined[id] {
			return false
		}
	}
	return true
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
