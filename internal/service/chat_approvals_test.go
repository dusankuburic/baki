package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// toolsRecordingProvider is a FakeProvider that keeps every request it was sent, so a test can assert which tools
// the model was offered on each turn, and that fails a turn on a cancelled context as a real provider does.
type toolsRecordingProvider struct {
	testutil.FakeProvider
	mu   sync.Mutex
	reqs []ai.Request
}

func (p *toolsRecordingProvider) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	return p.FakeProvider.Stream(ctx, req, onChunk)
}

func (p *toolsRecordingProvider) requests() []ai.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ai.Request(nil), p.reqs...)
}

// approvalTestService is newFixTestService (a real flow + analysis service around fixTestDoc, so apply_fix
// resolves a real finding and the applier is wired) completed for StreamChatMessage, with p registered as id.
func approvalTestService(t *testing.T, id string, p ai.Provider, hook applyFixFunc) (*ChatService, *testutil.CountingNotifier, *models.FlowDocument) {
	t.Helper()
	doc := fixTestDoc()
	svc, n := newFixTestService(doc, hook)
	svc.configDir = t.TempDir()
	svc.factory = keyedFactory()
	svc.settings = nilSettings{}
	svc.chatCtxCache = newChatContextCache()
	ai.RegisterProvider(id, func(string) ai.Provider { return p })
	return svc, n, doc
}

// applyFixCall is the model turn that calls apply_fix on fixTestDoc's unhandled-error finding.
func applyFixCall(id string) testutil.FakeTurn {
	return testutil.FakeTurn{ToolCalls: []ai.ToolCall{{ID: id, Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}}, TokensIn: 10, TokensOut: 5}
}

// toolResultFor returns the tool_result payload of the named tool, or nil.
func toolResultFor(n *testutil.CountingNotifier, name string) map[string]interface{} {
	types, payloads := eventTypes(n)
	for i, typ := range types {
		if typ == "tool_result" && payloads[i]["name"] == name {
			return payloads[i]
		}
	}
	return nil
}

// TestRunToolLoop_ChatOnlyTokenNeverSeesApplyFix: the tools offered to the model follow the stream's actor. A
// chat-only API token may start a stream, but its model is never offered apply_fix/apply_fixes (it used to be,
// and burnt a paid turn to be refused), and a call it makes anyway is refused before the applier (no approval
// card) and counted.
func TestRunToolLoop_ChatOnlyTokenNeverSeesApplyFix(t *testing.T) {
	prov := &toolsRecordingProvider{FakeProvider: testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		applyFixCall("t1"),
		{Text: "I cannot apply it here.", TokensIn: 8, TokensOut: 3},
	}}}
	svc, n, doc := approvalTestService(t, "approval-chat-only", prov, failingApplyHook(t))
	refusedBefore := promtest.ToFloat64(chatToolRefusedTotal.WithLabelValues("apply_fix", ai.ToolRefusedScope))

	id, err := svc.StreamChatMessage(scopedCtx(auth.ScopeChat), "user-1", doc, nil,
		models.ChatRequest{Provider: "approval-chat-only", UserMessage: "fix it", UseTools: true, ClientStreamID: uuid.NewString()})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	ctl := waitStreamFinished(t, svc, id)
	if !ctl.done {
		t.Fatalf("stream did not complete: %q", ctl.errMsg)
	}

	reqs := prov.requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 model turns, got %d", len(reqs))
	}
	for i, r := range reqs {
		var names []string
		for _, d := range r.Tools {
			names = append(names, d.Name)
		}
		offered := strings.Join(names, ",")
		if strings.Contains(offered, "apply_fix") {
			t.Errorf("turn %d offered a fix tool to a chat-only token: %s", i, offered)
		}
		if !strings.Contains(offered, "search_flow") || !strings.Contains(offered, "propose_fix") {
			t.Errorf("turn %d lost the read-only tools: %s", i, offered)
		}
	}
	res := toolResultFor(n, "apply_fix")
	if res == nil {
		t.Fatal("no tool_result for the refused apply_fix call")
	}
	if res["ok"] != false || !strings.Contains(res["summary"].(string), "not available in this session") {
		t.Errorf("apply_fix result = %v, want the session refusal", res)
	}
	if types, _ := eventTypes(n); indexOf(types, "fix_proposal") >= 0 {
		t.Error("an approval card was shown to a token that could never act on it")
	}
	if got := promtest.ToFloat64(chatToolRefusedTotal.WithLabelValues("apply_fix", ai.ToolRefusedScope)) - refusedBefore; got != 1 {
		t.Errorf("pad_chat_tool_refused_total{apply_fix,scope} += %v, want 1", got)
	}
}

// TestApprovals_ShutdownEmitsCancelled: a proposal pending when the server stops is resolved for the client
// before the stream's SERVER_RESTARTING error (the card used to hang until the slot was torn down), and a
// decision arriving afterwards finds nothing pending. Nothing is applied.
func TestApprovals_ShutdownEmitsCancelled(t *testing.T) {
	prov := &toolsRecordingProvider{FakeProvider: testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		applyFixCall("t1"),
		{Text: "never reached", TokensIn: 8, TokensOut: 3},
	}}}
	svc, n, doc := approvalTestService(t, "approval-shutdown", prov, failingApplyHook(t))
	cancelledBefore := promtest.ToFloat64(chatFixDecisionsTotal.WithLabelValues("cancelled"))

	id, err := svc.StreamChatMessage(context.Background(), "user-1", doc, nil,
		models.ChatRequest{Provider: "approval-shutdown", UserMessage: "fix it", UseTools: true, ClientStreamID: uuid.NewString()})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	// The card is up once the proposal is registered for the stream.
	var proposalID string
	deadline := time.Now().Add(5 * time.Second)
	for proposalID == "" {
		svc.pendingFixDecisions.Range(func(k, v any) bool {
			if v.(*fixDecisionState).streamID == id {
				proposalID = k.(string)
			}
			return proposalID == ""
		})
		if time.Now().After(deadline) {
			t.Fatal("no approval became pending")
		}
		time.Sleep(2 * time.Millisecond)
	}

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	types, payloads := eventTypes(n)
	cancelledAt, errorAt := -1, -1
	for i, typ := range types {
		switch {
		case typ == "fix_decision" && payloads[i]["status"] == "cancelled":
			cancelledAt = i
			if payloads[i]["proposalId"] != proposalID || payloads[i]["message"] == "" {
				t.Errorf("cancelled decision = %v, want proposal %s with a message", payloads[i], proposalID)
			}
		case typ == "error":
			errorAt = i
			if payloads[i]["code"] != ChatErrServerRestarting {
				t.Errorf("error code = %v, want %s", payloads[i]["code"], ChatErrServerRestarting)
			}
		}
	}
	if cancelledAt < 0 || errorAt < 0 || cancelledAt > errorAt {
		t.Fatalf("events %v: want fix_decision{cancelled} before the %s error", types, ChatErrServerRestarting)
	}
	if err := svc.ResolveFixDecision(id, proposalID, true, nil); !errors.Is(err, ErrNoPendingFix) {
		t.Errorf("a decision after the stop: err = %v, want ErrNoPendingFix", err)
	}
	if got := promtest.ToFloat64(chatFixDecisionsTotal.WithLabelValues("cancelled")) - cancelledBefore; got != 1 {
		t.Errorf("pad_chat_fix_decisions_total{cancelled} += %v, want 1", got)
	}
}

// TestAgentTurn_ForeignFunctionCallIsRefusedNotExecuted pins today's behaviour for an Azure agent turn: the loop
// runs with baki's tools on the request (the provider drops them for an agent), and a function_call the agent
// emits for a tool of its own is answered "unknown tool" and never reaches a registry handler. A gate that
// assumed "offered ⇒ allowed" would be wrong here; the execution-side check is what holds.
func TestAgentTurn_ForeignFunctionCallIsRefusedNotExecuted(t *testing.T) {
	prov := &toolsRecordingProvider{FakeProvider: testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "c1", Name: "foo", Input: []byte(`{"x":1}`)}}, TokensIn: 10, TokensOut: 5},
		{Text: "answer", TokensIn: 8, TokensOut: 3},
	}}}
	svc, n, doc := approvalTestService(t, "approval-agent", prov, failingApplyHook(t))
	otherBefore := promtest.ToFloat64(chatToolResultsTotal.WithLabelValues("other", "false"))

	id, err := svc.StreamChatMessage(context.Background(), "user-1", doc, nil,
		models.ChatRequest{Provider: "approval-agent", Model: "agent:Helper", UserMessage: "hi", ClientStreamID: uuid.NewString()})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	ctl := waitStreamFinished(t, svc, id)
	if !ctl.done || ctl.buffer.String() != "answer" {
		t.Fatalf("stream: done=%v text=%q err=%q", ctl.done, ctl.buffer.String(), ctl.errMsg)
	}
	reqs := prov.requests()
	if len(reqs) != 2 || len(reqs[0].Tools) == 0 {
		t.Fatalf("want 2 agent turns with the tool loop's tools on the request, got %d (tools on first: %d)", len(reqs), len(reqs[0].Tools))
	}
	res := toolResultFor(n, "foo")
	if res == nil || res["ok"] != false || res["summary"] != "error: unknown tool foo" {
		t.Errorf("foo result = %v, want the unknown-tool refusal", res)
	}
	if got := promtest.ToFloat64(chatToolResultsTotal.WithLabelValues("other", "false")) - otherBefore; got != 1 {
		t.Errorf("pad_chat_tool_results_total{other,false} += %v, want 1", got)
	}
	if types, _ := eventTypes(n); indexOf(types, "fix_proposal") >= 0 {
		t.Error("a foreign call must not reach an approval")
	}
}

// TestApprovals_EventShape pins the wire shape the ApprovalCard reads (ADR 0026): a single-fix proposal carries
// its flat fields (old clients) AND items[], plus tool, previewKind and requiredScope; a batch carries the same
// envelope with batch/count. The stored snapshot keeps tool and previewKind.
func TestApprovals_EventShape(t *testing.T) {
	doc := fixTestDoc()
	svc, _ := newFixTestService(doc, failingApplyHook(t))
	svc.fixDecisionWait = 20 * time.Millisecond
	var mu sync.Mutex
	var proposals []map[string]interface{}
	emit := func(eventType string, data map[string]interface{}) {
		if eventType == "fix_proposal" {
			mu.Lock()
			proposals = append(proposals, data)
			mu.Unlock()
		}
	}
	ap := svc.newFixApplier(doc, "stream-1", emit, nil, nil)
	if _, err := ap.ApplyFixWithApproval(context.Background(), fixTestProposal()); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.ApplyFixesWithApproval(context.Background(), []ai.FixProposal{fixTestProposal(), fixTestProposal()}); err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 2 {
		t.Fatalf("want 2 proposals, got %d", len(proposals))
	}
	for i, p := range proposals {
		if p["tool"] == nil || p["previewKind"] != ai.PreviewKindFixPatch || p["requiredScope"] != auth.ScopeWrite {
			t.Errorf("proposal %d envelope = tool:%v previewKind:%v requiredScope:%v", i, p["tool"], p["previewKind"], p["requiredScope"])
		}
		items, _ := p["items"].([]map[string]interface{})
		if len(items) != i+1 {
			t.Errorf("proposal %d: items = %v, want %d", i, p["items"], i+1)
			continue
		}
		if items[0]["ruleId"] != "unhandled-error" || items[0]["summary"] == "" || items[0]["id"] != "b1:unhandled-error" {
			t.Errorf("proposal %d: item = %v (want ruleId, summary and id = blockId:ruleId)", i, items[0])
		}
	}
	single, batch := proposals[0], proposals[1]
	if single["tool"] != "apply_fix" || single["ruleId"] != "unhandled-error" || single["blockLabel"] != "Call API" || single["batch"] != nil {
		t.Errorf("single proposal lost its flat fields: %v", single)
	}
	if batch["tool"] != "apply_fixes" || batch["batch"] != true || batch["count"] != 2 {
		t.Errorf("batch proposal envelope: %v", batch)
	}

	// The persisted record keeps the kind, so a reloaded conversation renders the right preview.
	_, fixes := journalTurnRecord([]journalEvent{
		{Type: "fix_proposal", Data: single},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": single["proposalId"], "status": "cancelled", "message": "stopped"}},
	})
	if len(fixes) != 1 || fixes[0].Tool != "apply_fix" || fixes[0].PreviewKind != ai.PreviewKindFixPatch {
		t.Errorf("snapshot = %+v, want tool and previewKind", fixes)
	}
	if fixes[0].Status != "cancelled" || len(fixes[0].Items) != 1 || fixes[0].Items[0].Status != "error" {
		t.Errorf("snapshot status = %q items=%+v, want cancelled with its item clamped to error", fixes[0].Status, fixes[0].Items)
	}
}
