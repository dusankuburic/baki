package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-provider-12: a context-length rejection that states the real window ("maximum context length is N tokens",
// *ai.ContextLimitError{Limit: N}) used to end the turn that received it — the window the turn was sized to was
// fixed at its start, and learning (alias providers only) helped only the next request. The turn now lowers its
// window to the stated one, truncates again and retries that call once; a rejection that states no window, or
// one the request cannot be made to fit, still ends the turn with CONTEXT_LIMIT.

// windowRetryStatedLimit is the window the scripted rejection states: far below the provider's own (FakeProvider's
// 1<<20 default, so the turn's first request keeps its whole history) and well above the current question plus the
// system prompt and tools, so a re-truncated request fits it.
const windowRetryStatedLimit = 6000

// windowRetryMode is one of the three paths a turn's provider call takes.
type windowRetryMode struct {
	name     string
	tools    bool // the provider does native function calling (runToolLoop), else the prompt-based loop
	useTools bool // the turn runs a tool loop at all
}

var windowRetryModes = []windowRetryMode{
	{name: "native tool loop", tools: true, useTools: true},
	{name: "prompt tool loop", tools: false, useTools: true},
	{name: "single call", tools: true, useTools: false},
}

// newWindowRetryChatService is newCtxBlockChatService whose provider chain (retry, audited) records usage rows in a
// FakeBackend.
func newWindowRetryChatService(t *testing.T, providerID string, p ai.Provider) (*ChatService, *testutil.FakeBackend) {
	t.Helper()
	be := testutil.NewFakeBackend()
	svc := newCtxBlockChatService(t, providerID, p)
	svc.factory = ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, be.SaveUsageMetric, nil)
	return svc, be
}

// windowRetryHistory is a stored thread of 24 messages, ~7k tokens on FakeProvider's len/4 estimate.
func windowRetryHistory() []models.ChatMessage {
	var history []models.ChatMessage
	for i := 0; i < 12; i++ {
		history = append(history,
			models.ChatMessage{ID: uuid.NewString(), Role: "user", Content: fmt.Sprintf("question %d: %s", i, strings.Repeat("why does the flow fail here ", 40))},
			models.ChatMessage{ID: uuid.NewString(), Role: "assistant", Content: fmt.Sprintf("answer %d: %s", i, strings.Repeat("because the loop never exits ", 40))},
		)
	}
	return history
}

// runWindowRetryTurn stores the history, streams one turn on providerID to its end and returns its control.
func runWindowRetryTurn(t *testing.T, svc *ChatService, providerID string, mode windowRetryMode) (*streamCtl, models.ChatRequest, []models.ChatMessage) {
	t.Helper()
	doc := parseCtxBlockDoc(t, ctxBlockSrc)
	history := windowRetryHistory()
	req := models.ChatRequest{Provider: providerID, Model: "fake-model", UserMessage: "and now?", MaxTokens: 500,
		UseTools: mode.useTools, ConversationID: uuid.NewString(), ClientStreamID: uuid.NewString()}
	if _, err := svc.saveConversation(context.Background(), doc.ID, req.ConversationKey(), history); err != nil {
		t.Fatal(err)
	}
	id, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	return waitStreamFinished(t, svc, id), req, history
}

// waitUsageRows waits for n usage rows, then a little longer for any extra one, and returns them all.
func waitUsageRows(t *testing.T, be *testutil.FakeBackend, n int) []*storageif.UsageMetric {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(be.SavedUsageMetrics()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d usage row(s) recorded, want %d", len(be.SavedUsageMetrics()), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	return be.SavedUsageMetrics()
}

// historyNotices is the dropped and keptFrom of every history_truncated notice the turn emitted, in order.
func historyNotices(svc *ChatService) (dropped, keptFrom []int) {
	types, payloads := eventTypes(svc.notifier.(*testutil.CountingNotifier))
	for i, typ := range types {
		if typ == "notice" && payloads[i]["kind"] == noticeHistoryTruncated {
			d, _ := payloads[i]["dropped"].(int)
			k, _ := payloads[i]["keptFrom"].(int)
			dropped, keptFrom = append(dropped, d), append(keptFrom, k)
		}
	}
	return dropped, keptFrom
}

// The rejected call (billed: the rejection carries usage) is retried at the stated window with older history
// dropped: the turn finishes, the user is told what was left out, the rejected call is one usage row and the
// done reports only the answering call.
func TestChatTurn_StatedWindowRejectionRetriesOnceReTruncated(t *testing.T) {
	for _, mode := range windowRetryModes {
		t.Run(mode.name, func(t *testing.T) {
			rejection := &ai.ContextLimitError{Limit: windowRetryStatedLimit}
			p := &testutil.FakeProvider{Tools: mode.tools, RequireScrubbed: t, Turns: []testutil.FakeTurn{
				{ChunkErr: rejection, TokensIn: 7},
				{Text: "answer", TokensIn: 3, TokensOut: 2},
			}}
			providerID := "window-retry-ok-" + strings.ReplaceAll(mode.name, " ", "-")
			svc, be := newWindowRetryChatService(t, providerID, p)
			ctl, req, history := runWindowRetryTurn(t, svc, providerID, mode)

			if !ctl.done || ctl.errCode != "" {
				t.Fatalf("turn = done %v, code %q (%s); want it answered after one re-truncated retry", ctl.done, ctl.errCode, ctl.errMsg)
			}
			reqs := p.Requests()
			if len(reqs) != 2 {
				t.Fatalf("provider calls = %d, want 2 (the rejected one and its retry)", len(reqs))
			}
			first, retry := len(reqs[0].Messages), len(reqs[1].Messages)
			if first != len(history)+1 {
				t.Fatalf("the first call sent %d messages, want the whole thread (%d): the test needs a window that fits it", first, len(history)+1)
			}
			if retry >= first {
				t.Fatalf("the retry sent %d messages, want fewer than the rejected call's %d", retry, first)
			}
			if est := windowRetryPromptEstimate(p, reqs[1]); est+reqs[1].MaxTokens > windowRetryStatedLimit {
				t.Errorf("the retry is %d prompt + %d output tokens, over the stated %d-token window", est, reqs[1].MaxTokens, windowRetryStatedLimit)
			}
			if last := reqs[1].Messages[len(reqs[1].Messages)-1]; last.Role != "user" || !strings.HasSuffix(last.Content, "and now?") {
				t.Errorf("the retry's last message is %s %.40q…, want the current question", last.Role, last.Content)
			}

			// The user is told once, with the count the retry left out; the stored answer records it.
			wantDropped := first - retry
			dropped, keptFrom := historyNotices(svc)
			if len(dropped) != 1 || dropped[0] != wantDropped || keptFrom[0] != wantDropped {
				t.Errorf("history_truncated notices dropped=%v keptFrom=%v, want one saying %d", dropped, keptFrom, wantDropped)
			}
			stored, err := svc.GetConversation(context.Background(), parseCtxBlockDoc(t, ctxBlockSrc).ID, req.ConversationKey())
			if err != nil {
				t.Fatal(err)
			}
			if last := stored[len(stored)-1]; last.Role != "assistant" || last.Truncated != wantDropped {
				t.Errorf("the stored answer records truncated=%d (role %s), want %d", last.Truncated, last.Role, wantDropped)
			}

			// Billing: one row for the rejected call, one for the answer; the done counts the answer only.
			rows := waitUsageRows(t, be, 2)
			if len(rows) != 2 {
				t.Fatalf("usage rows = %d, want 2 (the rejected call once, the answer once)", len(rows))
			}
			var rejected, answered int
			for _, r := range rows {
				switch {
				case r.PromptTokens == 7 && r.CompletionTokens == 0:
					rejected++
				case r.PromptTokens == 3 && r.CompletionTokens == 2:
					answered++
				}
			}
			if rejected != 1 || answered != 1 {
				t.Errorf("usage rows = %+v, want the rejected call (7/0) and the answer (3/2) once each", usageRowTokens(rows))
			}
			if ctl.tokensIn != 3 || ctl.tokensOut != 2 {
				t.Errorf("the turn's done reports %d/%d tokens, want the answering call's 3/2", ctl.tokensIn, ctl.tokensOut)
			}
			if mode.useTools {
				// The turn record counts provider calls, as the tools-unsupported retry does; the step budget is untouched.
				if n := ctl.turn.iterations.Load(); n != 2 {
					t.Errorf("the turn record counts %d iterations, want 2 (the rejected call and its retry)", n)
				}
			}
		})
	}
}

// A rejection that cannot be helped by a retry still ends the turn with CONTEXT_LIMIT, after no more than one retry.
func TestChatTurn_ContextRejectionsThatCannotRetryFail(t *testing.T) {
	cases := []struct {
		name  string
		turns []testutil.FakeTurn
		calls int
	}{
		// No stated window: nothing to size the retry to.
		{name: "no stated window", turns: []testutil.FakeTurn{{Err: ai.ErrContextLimit}}, calls: 1},
		// A stated window no smaller than the one the turn was sized to: the same request would be rejected again.
		{name: "stated window not smaller", turns: []testutil.FakeTurn{{Err: &ai.ContextLimitError{Limit: 1 << 21}}}, calls: 1},
		// A stated window the current question alone (plus the system prompt and tools) does not fit.
		{name: "stated window too small for the question", turns: []testutil.FakeTurn{{Err: &ai.ContextLimitError{Limit: 600}}}, calls: 1},
		// The retry is rejected too: once is the limit, whatever the second rejection states.
		{name: "retry rejected again", turns: []testutil.FakeTurn{
			{Err: &ai.ContextLimitError{Limit: windowRetryStatedLimit}},
			{Err: &ai.ContextLimitError{Limit: windowRetryStatedLimit - 1000}},
		}, calls: 2},
	}
	for _, mode := range windowRetryModes {
		for _, tc := range cases {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				p := &testutil.FakeProvider{Tools: mode.tools, RequireScrubbed: t, Turns: tc.turns,
					Fallback: &testutil.FakeTurn{Text: "answer", TokensIn: 3, TokensOut: 2}}
				providerID := "window-retry-fail-" + strings.ReplaceAll(mode.name+"-"+tc.name, " ", "-")
				svc, _ := newWindowRetryChatService(t, providerID, p)
				ctl, _, _ := runWindowRetryTurn(t, svc, providerID, mode)
				if ctl.done || ctl.errCode != ChatErrContextLimit {
					t.Errorf("turn = done %v, code %q (%s); want CONTEXT_LIMIT", ctl.done, ctl.errCode, ctl.errMsg)
				}
				if n := p.Calls(); n != tc.calls {
					t.Errorf("provider calls = %d, want %d", n, tc.calls)
				}
			})
		}
	}
}

// windowRetryPromptEstimate is FakeProvider's own estimate of a request's prompt (the truncation's arithmetic:
// system prompt, tool schemas, each message plus its framing).
func windowRetryPromptEstimate(p *testutil.FakeProvider, req ai.Request) int {
	n := p.EstimateTokens(req.SystemPrompt)
	for _, td := range req.Tools {
		n += p.EstimateTokens(string(td.InputSchema)) + 8
	}
	for _, m := range req.Messages {
		n += p.EstimateTokens(m.Content) + 4
		for _, tc := range m.ToolCalls {
			n += p.EstimateTokens(string(tc.Input))
		}
	}
	return n
}

func usageRowTokens(rows []*storageif.UsageMetric) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("%d/%d", r.PromptTokens, r.CompletionTokens)
	}
	return out
}
