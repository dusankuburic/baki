package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-core/ai/scrubber"
	"pad-core/logger"
)

// toolExecResult is one executed tool call of a turn, in CALL order (results are
// buffered and emitted sequentially so journal, SSE ordering and the msgs sequence stay
// deterministic regardless of execution concurrency).
type toolExecResult struct {
	id     string
	name   string
	result string
	took   time.Duration
}

// maxParallelTools bounds how many read-only tools of one turn execute at once; the
// read-only tools are in-memory scans (ms-fast), so this is a politeness bound, not a
// capacity one.
const maxParallelTools = 4

// guardedToolRun runs one tool call, converting a panic in the tool — model-controlled
// input reaching a bug in the tool implementation — into the error-shaped result tool
// failures already surface to the model, so the turn continues. Every path runs under it:
// a parallel goroutine runs outside the stream worker's recover (a panic there would kill
// the process), and on the sequential and prompt-loop paths the worker's recover would end
// the stream as interrupted instead of letting the model read the failure (R-tool-loop-7).
func guardedToolRun(ctx context.Context, call ai.ToolCall, run func() toolExecResult) (res toolExecResult) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(ctx).Warn("chat tool panicked", "tool", call.Name, "panic", r)
			recordChatToolResult(metricToolLabel(call.Name), false)
			res = toolExecResult{
				id:   call.ID,
				name: call.Name,
				// The panic value is model-visible text: it passes the scrubber like every tool result.
				result: scrubber.ScrubText(fmt.Sprintf("error: tool %s failed unexpectedly (%v) — try a different tool or answer without it", call.Name, r)),
				took:   time.Since(start),
			}
		}
	}()
	return run()
}

// execTurnTools runs a model turn's tool calls through the loop memory, emitting each
// call's tool (starting) / tool_result (finished) events itself and returning results in
// call order. All-read-only turns of more than one call execute concurrently (bounded by
// maxParallelTools), starting events up front, finished events in call order — journal
// and msgs stay deterministic. A turn containing a fix tool (apply_fix/apply_fixes: one
// blocks on a human decision for up to 60s, both mutate the document) is fully
// sequential, preserving the tool-status UX during the approval wait.
func (s *ChatService) execTurnTools(
	emit func(string, map[string]interface{}),
	ctl *streamCtl,
	ensureStarted func() bool,
	mem *loopToolMemory,
	tctx *ai.ToolContext,
	calls []ai.ToolCall,
) []toolExecResult {
	results := make([]toolExecResult, len(calls))
	ctl.setPhase(phaseTool)

	run := func(i int) toolExecResult {
		tc := calls[i]
		// Tool execution produces no provider chunks, and the idle watchdog counts ONLY
		// provider chunks + these touches as activity — without them a legitimately slow
		// tool (>90s) gets the healthy turn cancelled as "provider stopped responding".
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
		if ai.ToolRequiresApproval(tc.Name) {
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
			results[i] = guardedToolRun(tctx.Ctx, calls[i], func() toolExecResult { return run(i) })
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
				results[i] = guardedToolRun(tctx.Ctx, calls[i], func() toolExecResult { return run(i) })
			}(i)
		}
		wg.Wait()
		for i := start; i < end; i++ {
			emitToolResult(emit, calls[i].Name, results[i].result, results[i].took)
		}
	}
	return results
}
