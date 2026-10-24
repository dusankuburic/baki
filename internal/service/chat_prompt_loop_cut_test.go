package service

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// R-tool-loop-3: in marker mode a <tool_call> cut at the output cap matched nothing, so the loop emitted the
// partial JSON as the answer, ran no tool and recorded a success. The native loop refuses such a turn
// (truncatedToolTurn); the prompt loop now does the same, and never shows an unterminated marker.

func runMarkerLoop(t *testing.T, prov *testutil.FakeProvider) (*streamCtl, []capturedEvent) {
	t.Helper()
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, events := captureEvents()
	svc.runPromptToolLoop(context.Background(), prov, ai.Request{
		SystemPrompt: "You are an assistant.",
		Messages:     []ai.Message{{Role: "user", Content: "where is the API call?"}},
	}, "user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })
	return ctl, events()
}

func TestRunPromptToolLoop_ToolCallCutAtTheOutputLimitFails(t *testing.T) {
	prov := &testutil.FakeProvider{Turns: []testutil.FakeTurn{
		{Text: "Let me look\n<tool_call>\n{\"name\":\"search_flow\"", FinishReason: "length", TokensIn: 10, TokensOut: 4096},
	}}
	ctl, evs := runMarkerLoop(t, prov)
	last := evs[len(evs)-1]
	if last.typ != "error" || last.data["code"] != ChatErrOutputLimit {
		t.Fatalf("last event = %+v, want an error coded %s", last, ChatErrOutputLimit)
	}
	for _, ev := range evs {
		if ev.typ == "tool" || ev.typ == "tool_result" {
			t.Errorf("a tool ran for a call cut at the output limit: %+v", ev)
		}
		if ev.typ == "chunk" && strings.Contains(ev.data["text"].(string), "<tool_call>") {
			t.Errorf("a chunk carries the cut marker: %+v", ev)
		}
	}
	if strings.Contains(ctl.buffer.String(), "<tool_call>") {
		t.Errorf("the cut marker reached the client buffer: %q", ctl.buffer.String())
	}
	if prov.Calls() != 1 {
		t.Errorf("provider called %d times, want 1", prov.Calls())
	}
}

// A turn cut at the limit with a complete call is refused too: the model may have meant more (truncatedToolTurn).
func TestRunPromptToolLoop_CompleteCallAtTheOutputLimitDoesNotRun(t *testing.T) {
	prov := &testutil.FakeProvider{Turns: []testutil.FakeTurn{
		{Text: "<tool_call>\n{\"name\":\"search_flow\",\"input\":{\"query\":\"x\"}}\n</tool_call>\nand then", FinishReason: "max_tokens"},
	}}
	_, evs := runMarkerLoop(t, prov)
	last := evs[len(evs)-1]
	if last.typ != "error" || last.data["code"] != ChatErrOutputLimit {
		t.Fatalf("last event = %+v, want an error coded %s", last, ChatErrOutputLimit)
	}
	for _, ev := range evs {
		if ev.typ == "tool" {
			t.Errorf("a tool ran in a turn cut at the output limit: %+v", ev)
		}
	}
}

// An unterminated marker in a turn that ended normally is dropped from the answer, which still completes.
func TestRunPromptToolLoop_UnterminatedMarkerNeverReachesTheClient(t *testing.T) {
	prov := &testutil.FakeProvider{Turns: []testutil.FakeTurn{
		{Text: "The API call is in Main.\n<tool_call>\n{\"name\":\"get_bl", FinishReason: "stop"},
	}}
	ctl, evs := runMarkerLoop(t, prov)
	if !ctl.done {
		t.Fatalf("the turn did not complete: %+v", evs)
	}
	if got := ctl.buffer.String(); strings.Contains(got, "<tool_call>") || strings.Contains(got, "get_bl") || !strings.Contains(got, "The API call is in Main.") {
		t.Errorf("buffer = %q, want the answer without the unterminated marker", got)
	}
}
