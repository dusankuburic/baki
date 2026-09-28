package service

import (
	"fmt"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/metrics"
	"pad-core/logger"
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

// guardedToolRun runs one PARALLEL tool call, converting a panic in the tool —
// model-controlled input reaching a bug in the tool implementation — into the
// error-shaped result tool failures already surface to the model
// (ai.ExecuteTool's "error: ..." strings), so the turn continues. The parallel
// goroutines need this guard themselves: they run outside the stream worker's
// recover, so without it a panicking tool kills the whole process. The
// sequential path runs inside that recover and needs none.
func guardedToolRun(call ai.ToolCall, run func() toolExecResult) (res toolExecResult) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("chat tool panicked", "tool", call.Name, "panic", r)
			metrics.RecordChatToolResult(metricToolLabel(call.Name), false)
			res = toolExecResult{
				id:     call.ID,
				name:   call.Name,
				result: fmt.Sprintf("error: tool %s failed unexpectedly (%v) — try a different tool or answer without it", call.Name, r),
				took:   time.Since(start),
			}
		}
	}()
	return run()
}

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
				results[i] = guardedToolRun(calls[i], func() toolExecResult { return run(i) })
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
