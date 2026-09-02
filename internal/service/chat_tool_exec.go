package service

import (
	"sync"
	"time"

	"pad-analyzer/internal/ai"
)

// toolExecResult is one executed tool call of a turn, in CALL order (results
// are buffered and emitted sequentially so the journal, SSE ordering and the
// msgs sequence stay deterministic regardless of execution concurrency).
type toolExecResult struct {
	id     string
	name   string
	result string
	took   time.Duration
}

// maxParallelTools bounds how many read-only tools of one turn execute at
// once. The read-only tools are in-memory scans (ms-fast), so this is a
// politeness bound, not a capacity one.
const maxParallelTools = 4

// execTurnTools runs a model turn's tool calls through the loop memory,
// emitting each call's tool (starting) / tool_result (finished) events itself
// and returning the results in call order. A turn whose calls are ALL
// read-only and has more than one call executes concurrently (bounded by
// maxParallelTools) with the starting events emitted up front — the finished
// events still land in call order, so the journal and the msgs sequence stay
// deterministic. Any turn containing a fix tool (apply_fix/apply_fixes: one
// blocks on a human decision for up to 60s and both mutate the document) is
// fully sequential, preserving the tool-status UX during the approval wait.
func (s *ChatService) execTurnTools(
	emit func(string, map[string]interface{}),
	ctl *streamCtl,
	ensureStarted func() bool,
	mem *loopToolMemory,
	tctx *ai.ToolContext,
	calls []ai.ToolCall,
) []toolExecResult {
	results := make([]toolExecResult, len(calls))

	run := func(i int) toolExecResult {
		tc := calls[i]
		// Tool execution produces no provider chunks, and the idle watchdog
		// counts ONLY provider chunks + these touches as activity — without
		// them a legitimately slow tool (>90s) gets the healthy turn
		// cancelled as "provider stopped responding".
		ctl.touch()
		start := time.Now()
		result := mem.exec(tc.Name, tc.Input, tctx)
		ctl.touch()
		return toolExecResult{id: tc.ID, name: tc.Name, result: result, took: time.Since(start)}
	}
	emitTool := func(i int) {
		emit("tool", map[string]interface{}{"name": calls[i].Name, "label": ai.ToolLabel(calls[i].Name)})
	}

	allReadOnly := len(calls) > 1
	for _, tc := range calls {
		if isFixToolName(tc.Name) {
			allReadOnly = false
			break
		}
	}

	if !allReadOnly {
		for i := range calls {
			if !ensureStarted() {
				return nil
			}
			emitTool(i)
			results[i] = run(i)
			emitToolResult(emit, calls[i].Name, results[i].result, results[i].took)
		}
		return results
	}

	for start := 0; start < len(calls); start += maxParallelTools {
		end := min(start+maxParallelTools, len(calls))
		if !ensureStarted() {
			return nil
		}
		// Starting events first so the user sees what's in flight, then the
		// bounded-concurrent execution, then the finished events in order.
		for i := start; i < end; i++ {
			emitTool(i)
		}
		var wg sync.WaitGroup
		for i := start; i < end; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i] = run(i)
			}(i)
		}
		wg.Wait()
		for i := start; i < end; i++ {
			emitToolResult(emit, calls[i].Name, results[i].result, results[i].took)
		}
	}
	return results
}

// isFixToolName mirrors ai-side fix tool names (kept local to avoid growing
// the ai package's exported surface for one predicate).
func isFixToolName(name string) bool {
	return name == "apply_fix" || name == "apply_fixes"
}
