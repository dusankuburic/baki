package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// The prompt-marker twins of the step-limit tests in chat_tool_loop_steps_test.go: Copilot and every provider
// without native function calling route through runPromptToolLoop, which implements the same contract with
// <tool_call> markers in the text.

// markerCall is one tool call as a marker-format model writes it.
func markerCall(name, input string) string {
	return fmt.Sprintf("<tool_call>\n{\"name\": %q, \"input\": %s}\n</tool_call>", name, input)
}

// markerTurns scripts n distinct marker-only tool-call turns (distinct arguments, so the repetition guard
// stays out of it).
func markerTurns(n int) []testutil.FakeTurn {
	turns := make([]testutil.FakeTurn, n)
	for i := range turns {
		turns[i] = testutil.FakeTurn{Text: markerCall("search_flow", fmt.Sprintf(`{"query":"q%d"}`, i)), TokensIn: 1, TokensOut: 1}
	}
	return turns
}

func runPromptLoop(t *testing.T, stub ai.Provider, tctx *ai.ToolContext, prompt string) (*streamCtl, *countingEvents, *ChatService) {
	t.Helper()
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := startedCtl()
	ev := &countingEvents{}
	svc.runPromptToolLoop(context.Background(), stub, ai.Request{SystemPrompt: "You are an assistant.", Messages: []ai.Message{{Role: "user", Content: prompt}}},
		"user-1", tctx, ctl, func() bool { return true }, ev.emit, func() int { return 0 })
	return ctl, ev, svc
}

func TestRunPromptToolLoop_StepLimitAsksForAFinalAnswer(t *testing.T) {
	turns := append(markerTurns(maxToolIterations), testutil.FakeTurn{Text: "Here is what I found.", TokensIn: 1, TokensOut: 1})
	stub := &testutil.FakeProvider{Tools: false, RequireScrubbed: t, Turns: turns}

	ctl, _, _ := runPromptLoop(t, stub, &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, "look around")

	if !ctl.done {
		t.Fatalf("the turn did not finish: %s %s", ctl.errCode, ctl.errMsg)
	}
	if !strings.Contains(ctl.buffer.String(), "Here is what I found.") {
		t.Errorf("the final answer is missing: %q", ctl.buffer.String())
	}
	reqs := stub.Requests()
	if len(reqs) != maxToolIterations+1 {
		t.Fatalf("provider turns = %d, want %d (the steps plus one final answer)", len(reqs), maxToolIterations+1)
	}
	last := reqs[len(reqs)-1].SystemPrompt
	if !strings.Contains(strings.ToLower(last), "do not call any more tools") {
		t.Errorf("the final turn does not tell the model tools are over; system prompt: %q", last)
	}
	if !strings.Contains(last, "You are an assistant.") || !strings.Contains(last, "<tool_call>") {
		t.Errorf("the note displaced the caller's prompt or the tool protocol: %q", last)
	}
	if strings.Contains(strings.ToLower(reqs[0].SystemPrompt), "do not call any more tools") {
		t.Errorf("the note leaked into the first turn")
	}
}

func TestRunPromptToolLoop_StepLimitToolCallsAreNotRun(t *testing.T) {
	stub := &testutil.FakeProvider{Tools: false, RequireScrubbed: t, Turns: markerTurns(maxToolIterations + 1)}

	ctl, ev, _ := runPromptLoop(t, stub, &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, "look around")

	if ctl.done || ctl.errCode != ChatErrToolLoopLimit {
		t.Fatalf("want TOOL_LOOP_LIMIT, got done=%v code=%q msg=%q", ctl.done, ctl.errCode, ctl.errMsg)
	}
	if !strings.Contains(ctl.errMsg, "step limit") {
		t.Errorf("the message does not say the step limit ended the turn: %q", ctl.errMsg)
	}
	if stub.Calls() != maxToolIterations+1 {
		t.Errorf("provider turns = %d, want %d", stub.Calls(), maxToolIterations+1)
	}
	if got := ev.count("tool_result"); got != maxToolIterations {
		t.Errorf("tools executed = %d, want %d: the final turn's call must not run", got, maxToolIterations)
	}
	want := maxToolIterations + 1
	if ctl.tokensIn != want || ctl.tokensOut != want {
		t.Errorf("usage stored on the step-limit failure = %d/%d, want %d/%d", ctl.tokensIn, ctl.tokensOut, want, want)
	}
	if strings.Contains(ctl.buffer.String(), "<tool_call>") {
		t.Errorf("raw marker leaked to the client buffer: %q", ctl.buffer.String())
	}
}

func TestRunPromptToolLoop_AppliedFixesDoNotUseUpSteps(t *testing.T) {
	doc0, doc1, doc2, doc3 := threeFixDocs()
	hook := func(_ context.Context, _ *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		switch blockID {
		case "b1":
			return doc1, nil
		case "x2":
			return doc2, nil
		default:
			return doc3, nil
		}
	}
	svc, _ := newFixTestService(doc0, hook)
	ctl := startedCtl()
	ev := &countingEvents{}

	tctx := &ai.ToolContext{Ctx: context.Background(), Doc: doc0, RealDoc: doc0, Analysis: readOnlyAnalysis{svc.analysisCache}}
	tctx.Fixes = svc.newFixApplier(doc0, "stream-1", ev.emit, nil, func(fresh *models.FlowDocument) error {
		tctx.RealDoc, tctx.Doc = fresh, fresh
		return nil
	})

	stop := make(chan struct{})
	defer close(stop)
	go approveAll(svc, "stream-1", stop)

	call := func(name, input string) testutil.FakeTurn {
		return testutil.FakeTurn{Text: markerCall(name, input), TokensIn: 1, TokensOut: 1}
	}
	stub := &testutil.FakeProvider{Tools: false, RequireScrubbed: t, Turns: []testutil.FakeTurn{
		call("list_findings", `{}`),
		call("apply_fix", `{"blockId":"b1","ruleId":"unhandled-error"}`),
		call("list_findings", `{}`),
		call("apply_fix", `{"blockId":"x2","ruleId":"unhandled-error"}`),
		call("list_findings", `{}`),
		call("apply_fix", `{"blockId":"y3","ruleId":"unhandled-error"}`),
		call("list_findings", `{}`),
		{Text: "All three are fixed.", TokensIn: 1, TokensOut: 1},
	}}

	svc.runPromptToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "fix everything"}}},
		"user-1", tctx, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("the turn did not finish: %s %s", ctl.errCode, ctl.errMsg)
	}
	if !strings.Contains(ctl.buffer.String(), "All three are fixed.") {
		t.Errorf("final answer missing: %q", ctl.buffer.String())
	}

	// The listing after the first apply (turn 3's result, fed back with the 4th request) is the fresh one.
	reqs := stub.Requests()
	if len(reqs) < 4 {
		t.Fatalf("only %d provider turns", len(reqs))
	}
	msgs := reqs[3].Messages
	listing := msgs[len(msgs)-1].Content
	if !strings.Contains(listing, `<tool_result name="list_findings">`) {
		t.Fatalf("the 4th request does not end with the listing's result:\n%s", listing)
	}
	if strings.Contains(listing, "already answered") {
		t.Errorf("the listing after an apply was served from the loop memory:\n%s", listing)
	}
	if !strings.Contains(listing, "x2") || strings.Contains(listing, "block=b2") {
		t.Errorf("the listing after an apply does not carry the re-parsed block IDs:\n%s", listing)
	}
}

func TestRunPromptToolLoop_StepLimitWithNoAnswerFails(t *testing.T) {
	stub := &testutil.FakeProvider{Tools: false, RequireScrubbed: t, Turns: append(markerTurns(maxToolIterations), testutil.FakeTurn{TokensIn: 1, TokensOut: 1})}

	ctl, _, _ := runPromptLoop(t, stub, &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, "look around")

	if ctl.done || ctl.errCode != ChatErrToolLoopLimit {
		t.Fatalf("want TOOL_LOOP_LIMIT for an empty final answer, got done=%v code=%q msg=%q", ctl.done, ctl.errCode, ctl.errMsg)
	}
	if !strings.Contains(ctl.errMsg, "no answer") {
		t.Errorf("the message does not say the model gave no answer: %q", ctl.errMsg)
	}
}

// The marker loop used to fail the turn before showing the text the model wrote around its final-turn marker,
// so the same turn lost its answer here and kept it (then failed) in the native loop.
func TestRunPromptToolLoop_StepLimitKeepsTheAnswerBesideSkippedCalls(t *testing.T) {
	both := testutil.FakeTurn{Text: "Here is what I found.\n" + markerCall("search_flow", `{"query":"q7"}`), TokensIn: 1, TokensOut: 1}
	stub := &testutil.FakeProvider{Tools: false, RequireScrubbed: t, Turns: append(markerTurns(maxToolIterations), both)}

	ctl, ev, _ := runPromptLoop(t, stub, &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, "look around")

	if !ctl.done {
		t.Fatalf("the turn did not finish: %s %s", ctl.errCode, ctl.errMsg)
	}
	if !strings.Contains(ctl.buffer.String(), "Here is what I found.") || strings.Contains(ctl.buffer.String(), "<tool_call>") {
		t.Errorf("the answer is missing or the marker leaked: %q", ctl.buffer.String())
	}
	if got := ev.count("tool_result"); got != maxToolIterations {
		t.Errorf("tools executed = %d, want %d: the final turn's call must not run", got, maxToolIterations)
	}
	seq := ev.sequence()
	if len(seq) < 2 || seq[len(seq)-1] != "done" || seq[len(seq)-2] != "tool" {
		t.Fatalf("want a trail row then done at the end, got %v", seq)
	}
	if label, _ := ev.last("tool")["label"].(string); !strings.Contains(label, "skipped") {
		t.Errorf("the trail row does not say the call was skipped: %q", label)
	}
}
