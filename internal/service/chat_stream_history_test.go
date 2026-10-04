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

// An assistant message with blank content and no tool calls or fix
// proposals is not stored. An empty 'stop' answer saved as an empty assistant
// message used to brick the thread on Claude, whose Messages API rejects
// {"role":"assistant","content":""} on every later turn; a tool-bearing turn
// (its content is not the answer) survives.
func TestSaveConversation_DropsBlankAssistantMessages(t *testing.T) {
	svc := &ChatService{configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	msgs := []models.ChatMessage{
		{ID: "1", Role: "user", Content: "hi"},
		{ID: "2", Role: "assistant", Content: ""},
		{ID: "3", Role: "assistant", Content: "   "},
		{ID: "4", Role: "assistant", Content: "", ToolCalls: []models.ToolCallRecord{{Name: "search_flow", Ok: true}}},
		{ID: "5", Role: "assistant", Content: "", FixProposals: []models.FixProposalSnapshot{{ProposalID: "p1"}}},
		{ID: "6", Role: "user", Content: "again"},
	}
	if err := svc.SaveConversation(context.Background(), doc.ID, "flow", msgs); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}
	got, err := svc.GetConversation(context.Background(), doc.ID, "flow")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if fmt.Sprint(ids) != "[1 4 5 6]" {
		t.Errorf("stored ids = %v, want the blank text-only assistant turns dropped (1 4 5 6)", ids)
	}
}

// A refusal a Claude Mythos deployment gave an API-key credential is
// Entra-only copy, not the generic invalid-key text — and not the permission
// copy either (it also unwraps to ErrApiKeyInvalid, which the permission and
// auth cases both match).
func TestClassifyFailure_ClaudeMythosEntraOnly(t *testing.T) {
	ctl := &streamCtl{}
	wrapped := fmt.Errorf("azure ai foundry refused the API key for Claude deployment %q: %w", "claude:mythos", ai.ErrClaudeEntraOnly)
	code, msg := ctl.classifyFailure(context.Background(), wrapped)
	if code != ChatErrProviderAuth {
		t.Errorf("code = %q, want %q", code, ChatErrProviderAuth)
	}
	for _, want := range []string{"Claude Mythos deployments accept Microsoft Entra ID only", "switch the Azure AI Foundry credential to Microsoft Entra ID"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "API key for this provider is invalid") {
		t.Errorf("message %q must not be the generic invalid-key copy", msg)
	}
	if strings.Contains(msg, "service principal lacks permission") {
		t.Errorf("message %q must not be the permission copy", msg)
	}
}

// ActiveStreamFor resolves the caller's stream on a (flow, conversation)
// whose answer the stored history does not carry yet: a live stream once
// its question is stored (a client re-attaching before then saved a
// history without it), preferred over a finished one; a finished stream
// only while its cleanup — the answer persist — has not settled; nothing
// once it settled or left the retention set.
func TestActiveStreamFor(t *testing.T) {
	svc := &ChatService{}
	mk := func(owner, conv, id string) *streamCtl {
		c := &streamCtl{ownerID: owner, flowID: "f1", convKey: conv, streamID: id, settled: make(chan struct{})}
		c.userTurnStored.Store(true)
		return c
	}
	live := mk("u1", "c1", "s-live")
	done := mk("u1", "c1", "s-done")
	other := mk("u2", "c1", "s-other")
	otherConv := mk("u1", "c2", "s-conv")
	svc.finishedStreams.Store("s-done", done)
	svc.activeStreams.Store("s-live", live)
	svc.activeStreams.Store("s-other", other)
	svc.activeStreams.Store("s-conv", otherConv)

	if got := svc.ActiveStreamFor("u1", "f1", "c1"); got != "s-live" {
		t.Errorf("ActiveStreamFor = %q, want the live stream (a live stream beats a finished one)", got)
	}
	live.userTurnStored.Store(false)
	if got := svc.ActiveStreamFor("u1", "f1", "c1"); got != "s-done" {
		t.Errorf("ActiveStreamFor = %q, want the settling finished stream (the live one's question is not stored)", got)
	}
	svc.activeStreams.Delete("s-live")
	if got := svc.ActiveStreamFor("u1", "f1", "c1"); got != "s-done" {
		t.Errorf("ActiveStreamFor after finish = %q, want the finished stream while its answer is being stored", got)
	}
	close(done.settled)
	if got := svc.ActiveStreamFor("u1", "f1", "c1"); got != "" {
		t.Errorf("ActiveStreamFor after settling = %q, want empty (the reply is in the history; resuming would duplicate it)", got)
	}
	svc.finishedStreams.Delete("s-done")
	if got := svc.ActiveStreamFor("u1", "f1", "c1"); got != "" {
		t.Errorf("ActiveStreamFor after retention = %q, want empty", got)
	}
	// Scope isolation: another caller's stream, or the same caller's stream on
	// another conversation, is not reported.
	svc.activeStreams.Store("s-other2", mk("u2", "c1", "s-other2"))
	if got := svc.ActiveStreamFor("u1", "f1", "c1"); got != "" {
		t.Errorf("ActiveStreamFor = %q, want empty for another caller's stream", got)
	}
	if got := svc.ActiveStreamFor("u1", "f1", "c2"); got != "s-conv" {
		t.Errorf("ActiveStreamFor = %q, want the conversation's own stream", got)
	}
}

// The context preview reports the MODEL's window (the listing entry),
// not the provider-wide placeholder, and falls back to the provider-wide
// value for an unlisted model.
func TestPreviewContext_UsesModelWindow(t *testing.T) {
	p := &windowFakeProvider{FakeProvider: testutil.FakeProvider{}, providerLimit: 128000}
	const id = "preview-window-fake"
	ai.RegisterProvider(id, func(string) ai.Provider { return p })
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	svc := &ChatService{factory: factory, chatCtxCache: newChatContextCache()}
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}

	res, err := svc.PreviewContext(context.Background(), "u1", doc, nil, models.ChatRequest{Provider: id, Model: "big-window"})
	if err != nil {
		t.Fatalf("PreviewContext: %v", err)
	}
	if res.ContextLimit != 1<<20 {
		t.Errorf("listed model: ContextLimit = %d, want the model's 1<<20 window", res.ContextLimit)
	}
	res, err = svc.PreviewContext(context.Background(), "u1", doc, nil, models.ChatRequest{Provider: id, Model: "not-listed"})
	if err != nil {
		t.Fatalf("PreviewContext: %v", err)
	}
	if res.ContextLimit != 128000 {
		t.Errorf("unlisted model: ContextLimit = %d, want the provider-wide 128000", res.ContextLimit)
	}
}
