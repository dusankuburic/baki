package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// Review of R-provider-12: a truncation inside the tool loop — the window re-enforced after a large tool result,
// or the retry at a stated window — rewrote the history, and every rewrite stripped every replay record, this
// turn's included, and stopped recording them. Claude's thinking and the Responses API's reasoning items replay only
// on an append-only history, but Gemini's record is per message (the thought signatures of that message's own
// parts, keyed by call id), and Gemini 3 refuses a replayed function call of the current turn without its
// signature: a Gemini tool turn that had to drop older history failed with "start a new conversation". A record
// that stands alone now survives the truncation on every message kept.

// geminiShaped is a scripted provider under Gemini's id, so the loop treats its replay records as Gemini's.
type geminiShaped struct{ *testutil.FakeProvider }

func (geminiShaped) ID() string { return "gemini" }

// geminiItems is a Gemini-shaped replay record for one call.
func geminiItems(callID string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"calls":[{"id":%q,"signature":"sig-%s","providerId":true}]}`, callID, callID))
}

// replayHistory is older history (4 exchanges of ~200 estimated tokens) and the current question.
func replayHistory() []ai.Message {
	var msgs []ai.Message
	for i := 0; i < 4; i++ {
		msgs = append(msgs,
			ai.Message{Role: "user", Content: fmt.Sprintf("old question %d %s", i, strings.Repeat("x", 800))},
			ai.Message{Role: "assistant", Content: fmt.Sprintf("old answer %d %s", i, strings.Repeat("y", 800))})
	}
	return append(msgs, ai.Message{Role: "user", Content: "look around"})
}

// assertCallCarriesItems checks that req replays the first iteration's call with its Gemini record.
func assertCallCarriesItems(t *testing.T, req ai.Request, callID string) {
	t.Helper()
	for _, m := range req.Messages {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 || m.ToolCalls[0].ID != callID {
			continue
		}
		if m.ProviderItemsFrom != "gemini" || string(m.ProviderItems) != string(geminiItems(callID)) {
			t.Errorf("the replayed call %s carries items %q from %q, want its Gemini record", callID, m.ProviderItems, m.ProviderItemsFrom)
		}
		return
	}
	t.Fatalf("the request does not replay call %s: %d messages", callID, len(req.Messages))
}

func TestRunToolLoop_GeminiRecordsSurviveTruncation(t *testing.T) {
	call := ai.ToolCall{ID: "c1", Name: "search_flow", Input: json.RawMessage(`{"query":"q0"}`)}
	base := ai.Request{Messages: replayHistory(), Tools: ai.ToolDefinitionsFor(nil)}
	probe := &testutil.FakeProvider{}
	// The window the first request just fits (FakeProvider estimates len/4; no max tokens: a 4000-token reserve).
	fits := windowRetryPromptEstimate(probe, base) + contextReserve + 5

	cases := []struct {
		name  string
		ctx   int
		turns []testutil.FakeTurn
		// sent is the request that must replay the call: after the history was truncated.
		sent int
	}{
		{
			// The tool result pushes the second iteration past the window: older history goes.
			name: "window re-enforced after a tool result", ctx: fits,
			turns: []testutil.FakeTurn{
				{ToolCalls: []ai.ToolCall{call}, ProviderItems: geminiItems("c1"), TokensIn: 1, TokensOut: 1},
				{Text: "done", TokensIn: 1, TokensOut: 1},
			},
			sent: 1,
		},
		{
			// The second iteration is rejected with a stated window and retried, truncated to it.
			name: "retry at a stated window",
			turns: []testutil.FakeTurn{
				{ToolCalls: []ai.ToolCall{call}, ProviderItems: geminiItems("c1"), TokensIn: 1, TokensOut: 1},
				{Err: &ai.ContextLimitError{Limit: fits}},
				{Text: "done", TokensIn: 1, TokensOut: 1},
			},
			sent: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &testutil.FakeProvider{Tools: true, Ctx: tc.ctx, Turns: tc.turns}
			svc := &ChatService{analysisCache: &AnalysisService{}}
			ctl := startedCtl()
			ev := &countingEvents{}
			svc.runToolLoop(context.Background(), geminiShaped{fake}, base, "user-1",
				&ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

			if !ctl.done {
				t.Fatalf("the turn did not finish: %s %s", ctl.errCode, ctl.errMsg)
			}
			reqs := fake.Requests()
			if len(reqs) != tc.sent+1 {
				t.Fatalf("provider calls = %d, want %d", len(reqs), tc.sent+1)
			}
			if got, all := len(reqs[tc.sent].Messages), len(base.Messages)+2; got >= all {
				t.Fatalf("the request after the tool step sent %d messages, want older history dropped (fewer than %d)", got, all)
			}
			assertCallCarriesItems(t, reqs[tc.sent], "c1")
		})
	}
}

// A record that does not stand alone (another provider's, chained across turns) still goes with a rewrite.
func TestRunToolLoop_ChainedRecordsStillStopOnTruncation(t *testing.T) {
	call := ai.ToolCall{ID: "c1", Name: "search_flow", Input: json.RawMessage(`{"query":"q0"}`)}
	base := ai.Request{Messages: replayHistory(), Tools: ai.ToolDefinitionsFor(nil)}
	fits := windowRetryPromptEstimate(&testutil.FakeProvider{}, base) + contextReserve + 5
	fake := &testutil.FakeProvider{Tools: true, Ctx: fits, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{call}, ProviderItems: json.RawMessage(`[{"type":"thinking","signature":"s"}]`), TokensIn: 1, TokensOut: 1},
		{Text: "done", TokensIn: 1, TokensOut: 1},
	}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := startedCtl()
	svc.runToolLoop(context.Background(), fake, base, "user-1",
		&ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, (&countingEvents{}).emit, func() int { return 0 })
	if !ctl.done {
		t.Fatalf("the turn did not finish: %s %s", ctl.errCode, ctl.errMsg)
	}
	reqs := fake.Requests()
	if len(reqs) != 2 || len(reqs[1].Messages) >= len(base.Messages)+2 {
		t.Fatalf("want two calls, the second truncated; got %d", len(reqs))
	}
	for _, m := range reqs[1].Messages {
		if len(m.ProviderItems) > 0 {
			t.Errorf("a chained record was replayed on a rewritten history: %q", m.ProviderItems)
		}
	}
}

// Review of R-provider-12 (2): a provider-run tool (an agent's web search) that already went to the trail in the
// rejected call would be shown again by the retry: a rejection after a hosted tool event is not retried.
type hostedThenRejected struct {
	*testutil.FakeProvider
	window int // the window the rejection states
}

func (p hostedThenRejected) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	if p.Record(req) == 0 {
		onChunk(ai.Chunk{ToolEvent: &ai.ToolEvent{Name: "web_search_call", Label: "Agent · web search"}})
		onChunk(ai.Chunk{ToolEvent: &ai.ToolEvent{Name: "web_search_call", Label: "Agent · web search", Done: true, OK: true}})
		return &ai.ContextLimitError{Limit: p.window}
	}
	onChunk(ai.Chunk{Text: "answer"})
	onChunk(ai.Chunk{Done: true, TokensIn: 1, TokensOut: 1})
	return nil
}

func TestRunToolLoop_NoWindowRetryAfterAHostedToolEvent(t *testing.T) {
	base := ai.Request{Messages: replayHistory(), Tools: ai.ToolDefinitionsFor(nil)}
	// A stated window a re-truncated request would fit (older history dropped): only the hosted step stops the retry.
	p := hostedThenRejected{FakeProvider: &testutil.FakeProvider{Tools: true}, window: windowRetryPromptEstimate(&testutil.FakeProvider{}, base) + contextReserve - 100}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := startedCtl()
	ev := &countingEvents{}
	svc.runToolLoop(context.Background(), p, base, "user-1",
		&ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, ev.emit, func() int { return 0 })

	if ctl.done || ctl.errCode != ChatErrContextLimit {
		t.Errorf("turn = done %v, code %q; want CONTEXT_LIMIT, not a retry", ctl.done, ctl.errCode)
	}
	if n := len(p.Requests()); n != 1 {
		t.Errorf("provider calls = %d, want 1: the hosted step already reached the trail", n)
	}
	if starts, ends := ev.count("tool"), ev.count("tool_result"); starts != 1 || ends != 1 {
		t.Errorf("hosted step trail rows: %d starts, %d completions; want one of each", starts, ends)
	}
}
