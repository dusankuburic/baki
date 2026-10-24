package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-tool-loop-11: a viewer's stream (it needs only ChatUse) was offered apply_fix; the card was emitted and only
// the apply step refused, after the human was asked. The stream now resolves the caller's edit permission once,
// and the gate, the offer and the prompt follow it.
func TestStream_ViewerIsNotOfferedTheFixTools(t *testing.T) {
	fx := newParityFixture(t, "gate-viewer", true)
	fx.provider.Turns = []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "c1", Name: "apply_fix", Input: []byte(`{"blockId":"B1","ruleId":"hardcoded-credential"}`)}}},
		{Text: "I cannot change the flow; here is the change to make."},
	}
	fx.svc.flowEditCheck = func(context.Context, string) error { return ErrPermissionDenied }
	before := promtest.ToFloat64(chatToolRefusedTotal.WithLabelValues("apply_fix", ai.ToolRefusedFlowPermission))

	req := models.ChatRequest{Provider: "gate-viewer", Model: "fake-model", UserMessage: "fix it", UseTools: true, ClientStreamID: uuid.NewString()}
	id, err := fx.svc.StreamChatMessage(context.Background(), "u1", fx.doc, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	waitStreamFinished(t, fx.svc, id)

	reqs := fx.provider.Requests()
	if len(reqs) == 0 {
		t.Fatal("no request reached the provider")
	}
	for _, td := range reqs[0].Tools {
		if td.Name == "apply_fix" || td.Name == "apply_fixes" {
			t.Errorf("a viewer was offered %s", td.Name)
		}
	}
	if strings.Contains(reqs[0].SystemPrompt, "apply_fix") {
		t.Errorf("a viewer's system prompt names apply_fix:\n%s", reqs[0].SystemPrompt)
	}
	types, payloads := eventTypes(fx.svc.notifier.(*testutil.CountingNotifier))
	for i, typ := range types {
		if typ == "fix_proposal" {
			t.Errorf("a fix card was emitted on a viewer's stream: %v", payloads[i])
		}
	}
	if after := promtest.ToFloat64(chatToolRefusedTotal.WithLabelValues("apply_fix", ai.ToolRefusedFlowPermission)); after != before+1 {
		t.Errorf("flow_permission refusals went %v -> %v, want one more", before, after)
	}
	if len(reqs) < 2 || !strings.Contains(lastToolResult(reqs[1]), "not available in this session") {
		t.Errorf("the model did not read the refusal: %+v", reqs)
	}
}

// lastToolResult is the content of the last tool message a request carries.
func lastToolResult(req ai.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "tool" {
			return req.Messages[i].Content
		}
	}
	return ""
}

// An editor's stream is offered the fix tools and its prompt names them.
func TestStream_EditorIsOfferedTheFixTools(t *testing.T) {
	fx := newParityFixture(t, "gate-editor", true)
	req := models.ChatRequest{Provider: "gate-editor", Model: "fake-model", UserMessage: "fix it", UseTools: true, ClientStreamID: uuid.NewString()}
	id, err := fx.svc.StreamChatMessage(context.Background(), "u1", fx.doc, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	waitStreamFinished(t, fx.svc, id)
	reqs := fx.provider.Requests()
	names := []string{}
	for _, td := range reqs[0].Tools {
		names = append(names, td.Name)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "apply_fix") {
		t.Errorf("an editor was offered %s", got)
	}
	if !strings.Contains(reqs[0].SystemPrompt, "apply_fix") {
		t.Error("an editor's prompt does not name apply_fix")
	}
}

// Two requests that differ only in UseTools build different contexts: the prompt variant is in the cache key.
func TestContextCache_ToolAccessSplitsTheEntry(t *testing.T) {
	fx := newParityFixture(t, "gate-cache", true)
	var computed atomic.Int32
	orig := contextScrub
	contextScrub = func(s string) string { computed.Add(1); return orig(s) }
	t.Cleanup(func() { contextScrub = orig })

	ctx := context.Background()
	base := models.ChatRequest{Provider: "gate-cache", Model: "fake-model", UserMessage: "what is wrong?"}
	noTools, err := fx.svc.PreviewContext(ctx, "u1", fx.doc, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	withTools := base
	withTools.UseTools = true
	tools, err := fx.svc.PreviewContext(ctx, "u1", fx.doc, nil, withTools)
	if err != nil {
		t.Fatal(err)
	}
	if n := computed.Load(); n != 4 {
		t.Errorf("%d scrub calls, want two builds (4): the tool access is not in the cache key", n)
	}
	if strings.Contains(noTools.SystemPrompt, "propose_fix") || !strings.Contains(tools.SystemPrompt, "propose_fix") {
		t.Errorf("the prompts do not follow the tool access:\n--- no tools\n%s\n--- tools\n%s", noTools.SystemPrompt, tools.SystemPrompt)
	}
}
