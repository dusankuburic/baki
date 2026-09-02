package service

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// TestRunToolLoop_RepetitionReturnsCachedResult: a model re-emitting the
// IDENTICAL tool call every turn gets the cached result back (with a change-
// tactic note) instead of re-billing the execution; the loop still finishes.
func TestRunToolLoop_RepetitionReturnsCachedResult(t *testing.T) {
	// search_flow with identical args every turn (the classic loop-without-
	// progress failure); the model then answers.
	stub := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: []byte(`{"query":"xav"}`)}}, TokensIn: 1, TokensOut: 1},
		{ToolCalls: []ai.ToolCall{{ID: "t2", Name: "search_flow", Input: []byte(`{"query":"xav"}`)}}, TokensIn: 1, TokensOut: 1},
		{ToolCalls: []ai.ToolCall{{ID: "t3", Name: "search_flow", Input: []byte(`{"query":"xav"}`)}}, TokensIn: 1, TokensOut: 1},
		{Text: "done", TokensIn: 1, TokensOut: 1},
	}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{cancel: func() {}, started: make(chan struct{})}
	close(ctl.started)
	emit, _ := collectEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "hi"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("loop did not finish: %q", ctl.errMsg)
	}
	// The model's final turn must show the repetition note on repeat calls.
	if !strings.Contains(ctl.buffer.String(), "done") {
		t.Fatalf("final answer missing: %q", ctl.buffer.String())
	}
}

// TestLoopToolMemory_CacheAndNotes drives the memory directly: identical
// calls return the cached result plus a "change tactic" note; a declined
// fix is hard-blocked on re-request.
func TestLoopToolMemory_CacheAndNotes(t *testing.T) {
	mem := newLoopToolMemory()
	tctx := &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}

	first := mem.exec("search_flow", []byte(`{"query":"xav"}`), tctx)
	if strings.Contains(first, "already answered") {
		t.Fatalf("first call must be clean: %q", first)
	}
	second := mem.exec("search_flow", []byte(`{"query":"xav"}`), tctx)
	if !strings.Contains(second, "already answered") || !strings.Contains(second, "change tactic") {
		t.Errorf("repeat call missing the dedup note: %q", second)
	}
	if !strings.Contains(second, first) {
		t.Errorf("repeat call must carry the cached result, got %q", second)
	}
	// A different query is a different signature — no note.
	if got := mem.exec("search_flow", []byte(`{"query":"other"}`), tctx); strings.Contains(got, "already answered") {
		t.Errorf("different args wrongly deduped: %q", got)
	}
}

// TestLoopToolMemory_DeclineBlock: once the user declines a fix, the same
// exact apply_fix request is refused WITHOUT re-prompting the user.
func TestLoopToolMemory_DeclineBlock(t *testing.T) {
	mem := newLoopToolMemory()

	// Simulate a declined apply: seed the memory as the applier's result would.
	input := []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)
	mem.results["apply_fix"+"\x00"+string(input)] = fixDeclinedMarker + " this fix — nothing was changed."
	mem.declined["apply_fix"+"\x00"+string(input)] = true

	got := mem.exec("apply_fix", input, &ai.ToolContext{Ctx: context.Background()})
	if !strings.Contains(got, "error:") || !strings.Contains(got, "DECLINED") {
		t.Errorf("declined-fix re-request not refused: %q", got)
	}
	// Same for batch applies.
	batchInput := []byte(`{"targets":[{"blockId":"b1","ruleId":"r"}]}`)
	mem.declined["apply_fixes"+"\x00"+string(batchInput)] = true
	got = mem.exec("apply_fixes", batchInput, &ai.ToolContext{Ctx: context.Background()})
	if !strings.Contains(got, "error:") {
		t.Errorf("declined-batch re-request not refused: %q", got)
	}
}
