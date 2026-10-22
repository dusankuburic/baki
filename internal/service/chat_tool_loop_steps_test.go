package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// countingEvents records every event, safely: tool execution can emit from more than one goroutine.
type countingEvents struct {
	mu    sync.Mutex
	types []string
	data  []map[string]any
}

func (c *countingEvents) emit(typ string, data map[string]any) {
	c.mu.Lock()
	c.types = append(c.types, typ)
	c.data = append(c.data, data)
	c.mu.Unlock()
}

func (c *countingEvents) count(typ string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.types {
		if t == typ {
			n++
		}
	}
	return n
}

// sequence is the event types in emission order.
func (c *countingEvents) sequence() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.types...)
}

// last is the data of the latest event of typ; nil when none was emitted.
func (c *countingEvents) last(typ string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.types) - 1; i >= 0; i-- {
		if c.types[i] == typ {
			return c.data[i]
		}
	}
	return nil
}

func startedCtl() *streamCtl {
	ctl := &streamCtl{cancel: func() {}, started: make(chan struct{})}
	close(ctl.started)
	return ctl
}

// searchTurns scripts n distinct tool-call turns (distinct arguments, so the repetition guard stays out of it).
func searchTurns(n int) []testutil.FakeTurn {
	turns := make([]testutil.FakeTurn, n)
	for i := range turns {
		turns[i] = testutil.FakeTurn{
			ToolCalls: []ai.ToolCall{{ID: fmt.Sprintf("t%d", i), Name: "search_flow", Input: fmt.Appendf(nil, `{"query":"q%d"}`, i)}},
			TokensIn:  1, TokensOut: 1,
		}
	}
	return turns
}

// TestRunToolLoop_StepLimitAsksForAFinalAnswer pins R-tool-loop-4: reaching the step limit used to fail the
// whole turn with TOOL_LOOP_LIMIT, throwing away every billed step and gathered result. The loop now asks for
// one final answer with tools ruled out, and a model that gives one finishes normally.
func TestRunToolLoop_StepLimitAsksForAFinalAnswer(t *testing.T) {
	turns := append(searchTurns(maxToolIterations), testutil.FakeTurn{Text: "Here is what I found.", TokensIn: 1, TokensOut: 1})
	stub := &testutil.FakeProvider{Tools: true, Turns: turns}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := startedCtl()
	ev := &countingEvents{}

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "look around"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

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
	if last := strings.ToLower(reqs[len(reqs)-1].SystemPrompt); !strings.Contains(last, "do not call any more tools") {
		t.Errorf("the final turn does not tell the model tools are over; system prompt: %q", reqs[len(reqs)-1].SystemPrompt)
	}
	if strings.Contains(strings.ToLower(reqs[0].SystemPrompt), "do not call any more tools") {
		t.Errorf("the note leaked into the first turn")
	}
}

// TestRunToolLoop_StepLimitToolCallsAreNotRun: a model that still calls tools on the final turn gets nothing
// executed, and the turn ends with TOOL_LOOP_LIMIT saying the step limit ended it.
func TestRunToolLoop_StepLimitToolCallsAreNotRun(t *testing.T) {
	stub := &testutil.FakeProvider{Tools: true, Turns: searchTurns(maxToolIterations + 1)}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := startedCtl()
	ev := &countingEvents{}

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "look around"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

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
}

// threeFixDocs are a flow with three fixable HTTP actions and the re-parses after wrapping them in an error
// handler one by one: each re-parse mints fresh block IDs and moves what follows down, as the real parser does.
// The actions are named as the parser names them, from the type: all three are "Invoke Url".
func threeFixDocs() (doc0, doc1, doc2, doc3 *models.FlowDocument) {
	http := func(id string, line int) models.Block {
		return models.Block{ID: id, Name: "Invoke Url", Type: models.BlockTypeAction, RawType: "HTTPClient.InvokeUrl", LineNumber: line,
			Properties: map[string]string{"Url": "https://example.com/" + id}}
	}
	handled := func(id string, line int, inner models.Block) models.Block {
		return models.Block{ID: id, Name: "On Block Error", Type: models.BlockTypeErrorHandler, RawType: "OnBlockError", LineNumber: line,
			Children: []models.Block{inner}}
	}
	mk := func(blocks ...models.Block) *models.FlowDocument {
		d := &models.FlowDocument{ID: "flow-1", Name: "Main", Subflows: []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: blocks}}}
		d.RebuildIndexes()
		return d
	}
	doc0 = mk(http("b1", 3), http("b2", 6), http("b3", 9))
	doc1 = mk(handled("h1", 3, http("x1", 4)), http("x2", 8), http("x3", 11))
	doc2 = mk(handled("h1", 3, http("x1", 4)), handled("h2", 8, http("y2", 9)), http("y3", 13))
	doc3 = mk(handled("h1", 3, http("x1", 4)), handled("h2", 8, http("y2", 9)), handled("h3", 13, http("z3", 14)))
	return
}

// approveAll resolves every fix decision the stream registers until stop is closed.
func approveAll(svc *ChatService, streamID string, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(2 * time.Millisecond):
		}
		svc.pendingFixDecisions.Range(func(k, v any) bool {
			if st := v.(*fixDecisionState); st.streamID == streamID {
				_ = svc.ResolveFixDecision(streamID, k.(string), true, nil)
			}
			return true
		})
	}
}

// TestRunToolLoop_AppliedFixesDoNotUseUpSteps pins R-tool-loop-4 and R-tool-loop-1 together. "Fix everything"
// is list, apply, list, apply…: three approved applies with a listing between each used to exceed the six
// steps and end in TOOL_LOOP_LIMIT after the fixes were written. And the listing after an apply was served
// from the loop memory — the pre-fix list, with block IDs the re-parse had retired.
func TestRunToolLoop_AppliedFixesDoNotUseUpSteps(t *testing.T) {
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
	tctx.Fixes = svc.newFixApplier(doc0, "stream-1", ev.emit, nil, func(fresh *models.FlowDocument) {
		tctx.RealDoc, tctx.Doc = fresh, fresh
	})

	stop := make(chan struct{})
	defer close(stop)
	go approveAll(svc, "stream-1", stop)

	call := func(id, name, input string) testutil.FakeTurn {
		return testutil.FakeTurn{ToolCalls: []ai.ToolCall{{ID: id, Name: name, Input: []byte(input)}}, TokensIn: 1, TokensOut: 1}
	}
	stub := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		call("l1", "list_findings", `{}`),
		call("a1", "apply_fix", `{"blockId":"b1","ruleId":"unhandled-error"}`),
		call("l2", "list_findings", `{}`),
		call("a2", "apply_fix", `{"blockId":"x2","ruleId":"unhandled-error"}`),
		call("l3", "list_findings", `{}`),
		call("a3", "apply_fix", `{"blockId":"y3","ruleId":"unhandled-error"}`),
		call("l4", "list_findings", `{}`),
		{Text: "All three are fixed.", TokensIn: 1, TokensOut: 1},
	}}

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "fix everything"}}},
		"user-1", tctx, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("the turn did not finish: %s %s", ctl.errCode, ctl.errMsg)
	}
	if !strings.Contains(ctl.buffer.String(), "All three are fixed.") {
		t.Errorf("final answer missing: %q", ctl.buffer.String())
	}

	// The listing after the first apply (turn 3's result, sent with the 4th request) is the fresh one.
	reqs := stub.Requests()
	if len(reqs) < 4 {
		t.Fatalf("only %d provider turns", len(reqs))
	}
	msgs := reqs[3].Messages
	listing := msgs[len(msgs)-1].Content
	if strings.Contains(listing, "already answered") {
		t.Errorf("the listing after an apply was served from the loop memory:\n%s", listing)
	}
	if !strings.Contains(listing, "x2") || strings.Contains(listing, "block=b2") {
		t.Errorf("the listing after an apply does not carry the re-parsed block IDs:\n%s", listing)
	}
}

// toolsRefusedAt wraps FakeProvider so that one Stream call (1-based) fails with ErrToolsUnsupported while
// tools are still offered, as a served model that rejects the tool schema does; the retry without tools gets
// the scripted turn.
type toolsRefusedAt struct {
	testutil.FakeProvider
	call int
}

func (p *toolsRefusedAt) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	if p.Calls() == p.call-1 && len(req.Tools) > 0 {
		return ai.ErrToolsUnsupported
	}
	return p.FakeProvider.Stream(ctx, req, onChunk)
}

// TestRunToolLoop_StepLimitNoteIsAddedOnceAfterAToolsRetry: the final turn keeps the tool definitions, so a
// model that rejects them there is retried without tools on the same pass. The note went onto the loop's
// request on every final pass, so the retry carried it twice.
func TestRunToolLoop_StepLimitNoteIsAddedOnceAfterAToolsRetry(t *testing.T) {
	turns := append(searchTurns(maxToolIterations), testutil.FakeTurn{Text: "Here is what I found.", TokensIn: 1, TokensOut: 1})
	stub := &toolsRefusedAt{FakeProvider: testutil.FakeProvider{Tools: true, Turns: turns}, call: maxToolIterations + 1}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := startedCtl()
	ev := &countingEvents{}

	svc.runToolLoop(context.Background(), stub, ai.Request{SystemPrompt: "You are an assistant.", Messages: []ai.Message{{Role: "user", Content: "look around"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("the turn did not finish: %s %s", ctl.errCode, ctl.errMsg)
	}
	reqs := stub.Requests()
	last := reqs[len(reqs)-1]
	if len(last.Tools) != 0 {
		t.Errorf("the retry after the rejection still offered %d tools", len(last.Tools))
	}
	if got := strings.Count(last.SystemPrompt, stepLimitNote); got != 1 {
		t.Errorf("the step-limit note appears %d times in the retried final turn, want once:\n%s", got, last.SystemPrompt)
	}
}

// TestRunToolLoop_StepLimitFailureCarriesUsage: the step-limit failure comes after seven billed provider
// turns; its error reports them like the other billed failures, instead of a bare error with no usage.
func TestRunToolLoop_StepLimitFailureCarriesUsage(t *testing.T) {
	stub := &testutil.FakeProvider{Tools: true, Turns: searchTurns(maxToolIterations + 1)}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := startedCtl()
	ev := &countingEvents{}

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "look around"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

	if ctl.errCode != ChatErrToolLoopLimit {
		t.Fatalf("want TOOL_LOOP_LIMIT, got done=%v code=%q msg=%q", ctl.done, ctl.errCode, ctl.errMsg)
	}
	want := maxToolIterations + 1
	if ctl.tokensIn != want || ctl.tokensOut != want {
		t.Errorf("usage stored on the step-limit failure = %d/%d, want %d/%d", ctl.tokensIn, ctl.tokensOut, want, want)
	}
	if data := ev.last("error"); data["tokensIn"] != want || data["tokensOut"] != want {
		t.Errorf("the error event does not carry the billed usage: %v", data)
	}
}

// TestRunToolLoop_OutputLimitOnTheFinalTurnIsNotTheStepLimit: a final turn cut at the token cap in the middle
// of a tool call is the output-limit failure it is on every other turn, not a model ignoring the note.
func TestRunToolLoop_OutputLimitOnTheFinalTurnIsNotTheStepLimit(t *testing.T) {
	cut := testutil.FakeTurn{ToolCalls: []ai.ToolCall{{ID: "t7", Name: "search_flow", Input: []byte(`{"query":"q7"}`)}}, FinishReason: "max_tokens", TokensIn: 1, TokensOut: 1}
	stub := &testutil.FakeProvider{Tools: true, Turns: append(searchTurns(maxToolIterations), cut)}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := startedCtl()
	ev := &countingEvents{}

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "look around"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

	if ctl.done || ctl.errCode != ChatErrOutputLimit {
		t.Errorf("want OUTPUT_LIMIT for a final turn cut mid-call, got done=%v code=%q msg=%q", ctl.done, ctl.errCode, ctl.errMsg)
	}
}

// TestRunToolLoop_StepLimitWithNoAnswerFails: a model that obeys the note but says nothing (it meant only to
// call a tool) used to end the turn with done and an empty answer after the tool trail.
func TestRunToolLoop_StepLimitWithNoAnswerFails(t *testing.T) {
	stub := &testutil.FakeProvider{Tools: true, Turns: append(searchTurns(maxToolIterations), testutil.FakeTurn{TokensIn: 1, TokensOut: 1})}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := startedCtl()
	ev := &countingEvents{}

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "look around"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

	if ctl.done || ctl.errCode != ChatErrToolLoopLimit {
		t.Fatalf("want TOOL_LOOP_LIMIT for an empty final answer, got done=%v code=%q msg=%q", ctl.done, ctl.errCode, ctl.errMsg)
	}
	if !strings.Contains(ctl.errMsg, "no answer") {
		t.Errorf("the message does not say the model gave no answer: %q", ctl.errMsg)
	}
}

// TestRunToolLoop_StepLimitKeepsTheAnswerBesideSkippedCalls: a model that writes its answer and still emits a
// tool call on the final turn has the answer kept (it already streamed) and the call skipped, with a trail row
// saying so; failing the turn after the text threw the answer away.
func TestRunToolLoop_StepLimitKeepsTheAnswerBesideSkippedCalls(t *testing.T) {
	both := testutil.FakeTurn{Text: "Here is what I found.", ToolCalls: []ai.ToolCall{{ID: "t7", Name: "search_flow", Input: []byte(`{"query":"q7"}`)}}, TokensIn: 1, TokensOut: 1}
	stub := &testutil.FakeProvider{Tools: true, Turns: append(searchTurns(maxToolIterations), both)}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := startedCtl()
	ev := &countingEvents{}

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "look around"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("the turn did not finish: %s %s", ctl.errCode, ctl.errMsg)
	}
	if !strings.Contains(ctl.buffer.String(), "Here is what I found.") {
		t.Errorf("the answer is missing: %q", ctl.buffer.String())
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
