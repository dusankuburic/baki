package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
)

// TestGuardedToolRun_ConvertsPanicToErrorResult: a panic in a parallel tool
// (model-controlled input reaching a tool bug) becomes the error-shaped result
// tool failures already surface to the model — the goroutine running it is
// bare (outside the stream worker's recover), so without the conversion the
// panic would kill the process.
func TestGuardedToolRun_ConvertsPanicToErrorResult(t *testing.T) {
	call := ai.ToolCall{ID: "t7", Name: "search_flow", Input: []byte(`{"query":"x"}`)}
	res := guardedToolRun(call, func() toolExecResult {
		panic("index out of range")
	})
	if res.id != "t7" || res.name != "search_flow" {
		t.Errorf("result must keep the call's identity, got %+v", res)
	}
	if !strings.HasPrefix(res.result, "error:") || !strings.Contains(res.result, "search_flow") {
		t.Errorf("result must be an error string naming the tool, got %q", res.result)
	}
	if res.took < 0 {
		t.Errorf("took must be measured, got %v", res.took)
	}
}

// TestGuardedToolRun_PassesSuccessThrough: the guard must not alter a normal
// result.
func TestGuardedToolRun_PassesSuccessThrough(t *testing.T) {
	call := ai.ToolCall{ID: "t1", Name: "get_block"}
	want := toolExecResult{id: "t1", name: "get_block", result: "block b1", took: 3 * time.Millisecond}
	got := guardedToolRun(call, func() toolExecResult { return want })
	if got.id != want.id || got.name != want.name || got.result != want.result || got.took != want.took {
		t.Errorf("guarded run altered the result: %+v, want %+v", got, want)
	}
}

// TestExecTurnTools_ParallelReadonlyCallsStayInCallOrder exercises the
// parallel branch end to end with real tools: more than one call, all
// read-only, results buffered and returned in call order.
func TestExecTurnTools_ParallelReadonlyCallsStayInCallOrder(t *testing.T) {
	doc := fixTestDoc()
	tctx := &ai.ToolContext{Ctx: context.Background(), Doc: doc, RealDoc: doc}
	ctl := &streamCtl{cancel: func() {}, started: make(chan struct{})}
	close(ctl.started)
	mem := newLoopToolMemory()

	calls := []ai.ToolCall{
		{ID: "t1", Name: "search_flow", Input: []byte(`{"query":"api"}`)},
		{ID: "t2", Name: "get_block", Input: []byte(`{"blockId":"b1"}`)},
	}
	var emitted []string
	// execTurnTools does not read the receiver; a bare service is enough.
	results := (&ChatService{}).execTurnTools(
		func(eventType string, _ map[string]interface{}) { emitted = append(emitted, eventType) },
		ctl, func() bool { return true }, mem, tctx, calls)

	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	for i, want := range []string{"t1", "t2"} {
		if results[i].id != want {
			t.Errorf("results[%d].id = %q, want %q (call order must be preserved)", i, results[i].id, want)
		}
		if results[i].result == "" {
			t.Errorf("results[%d] empty — the tool must answer", i)
		}
		if results[i].took < 0 {
			t.Errorf("results[%d].took = %v", i, results[i].took)
		}
	}
	if len(emitted) != 4 { // 2 tool-starting + 2 tool_result events
		t.Errorf("emitted %v, want 4 tool events", emitted)
	}
}
