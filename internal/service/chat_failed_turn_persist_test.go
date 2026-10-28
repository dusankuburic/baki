package service

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-approvals-4: a turn that fails after it changed the flow, ran tools or streamed text is still stored, so the
// conversation keeps the record of what happened.

// failedTurnService is a cloud-mode chat service whose stored conversations live in a fake backend, with the fix
// tools wired over doc (the apply hook returns a flow without the finding).
func failedTurnService(t *testing.T, provider string, p ai.Provider, doc *models.FlowDocument) *ChatService {
	t.Helper()
	ai.RegisterProvider(provider, func(string) ai.Provider { return p })
	svc, _ := newFixTestService(doc, func(_ context.Context, d *models.FlowDocument, _, _, _, _, _ string) (*models.FlowDocument, error) {
		return commentDoc(d.ID), nil
	})
	svc.configDir = t.TempDir()
	svc.factory = ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	svc.settings = nilSettings{}
	svc.chatCtxCache = newChatContextCache()
	svc.backend = testutil.NewFakeBackend()
	svc.mode = config.ModeCloud
	svc.flowEditCheck = func(context.Context, string) error { return nil }
	return svc
}

// streamTurn runs one server-owned turn (no history sent) to its end, approving every card it shows.
func streamTurn(t *testing.T, svc *ChatService, doc *models.FlowDocument, provider string, tools bool) (models.ChatRequest, *streamCtl) {
	t.Helper()
	req := models.ChatRequest{
		Provider: provider, UserMessage: "fix it", UseTools: tools, ConversationID: uuid.NewString(),
		UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString(),
	}
	id, err := svc.StreamChatMessage(context.Background(), "test", doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			svc.pendingFixDecisions.Range(func(k, v any) bool {
				if v.(*fixDecisionState).streamID == id {
					_ = svc.ResolveFixDecision(id, k.(string), true, nil)
				}
				return true
			})
		}
	}()
	svc.BeginStream(context.Background(), id)
	return req, waitStreamFinished(t, svc, id)
}

// storedAnswer is the turn's assistant message as GET conversation returns it.
func storedAnswer(t *testing.T, svc *ChatService, flowID string, req models.ChatRequest) *models.ChatMessage {
	t.Helper()
	history, err := svc.GetConversation(context.Background(), flowID, req.ConversationKey())
	if err != nil {
		t.Fatal(err)
	}
	for i := range history {
		if history[i].ID == req.AssistantMessageID {
			return &history[i]
		}
	}
	t.Fatalf("no stored answer %s in %+v", req.AssistantMessageID, history)
	return nil
}

// TestFailedTurn_AppliedFixIsStored: the model applies an approved fix, then the provider fails on the next
// iteration. The flow changed, so the turn is stored with the applied card, finish reason "error" and the code.
func TestFailedTurn_AppliedFixIsStored(t *testing.T) {
	doc := fixTestDoc()
	p := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}}, TokensIn: 10, TokensOut: 5},
		{Text: "Fixed. Next I", ChunkErr: fmt.Errorf("stream: %w", ai.ErrProviderDown)},
	}}
	svc := failedTurnService(t, "r-approvals-4-fix", p, doc)
	req, ctl := streamTurn(t, svc, doc, "r-approvals-4-fix", true)
	if ctl.errMsg == "" {
		t.Fatalf("the turn did not fail (done=%v)", ctl.done)
	}

	answer := storedAnswer(t, svc, doc.ID, req)
	if answer.FinishReason != "error" || answer.ErrorCode != ctl.errCode || answer.ErrorCode == "" {
		t.Errorf("stored finish %q code %q, want error and %q", answer.FinishReason, answer.ErrorCode, ctl.errCode)
	}
	if len(answer.FixProposals) != 1 || answer.FixProposals[0].Status != "applied" {
		t.Fatalf("stored fix records %+v, want the applied card", answer.FixProposals)
	}
	if !strings.Contains(answer.Content, "Fixed. Next I") || !strings.Contains(answer.Content, ctl.errMsg) {
		t.Errorf("stored content %q, want the partial text and the error line", answer.Content)
	}
}

// TestFailedTurn_PartialTextIsStored: a plain answer that fails after text streamed keeps that text, with the
// finish reason and the error code.
func TestFailedTurn_PartialTextIsStored(t *testing.T) {
	doc := fixTestDoc()
	p := &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "The flow starts by", ChunkErr: fmt.Errorf("stream: %w", ai.ErrProviderDown)}}}
	svc := failedTurnService(t, "r-approvals-4-text", p, doc)
	req, ctl := streamTurn(t, svc, doc, "r-approvals-4-text", false)
	if ctl.errMsg == "" {
		t.Fatalf("the turn did not fail (done=%v)", ctl.done)
	}

	answer := storedAnswer(t, svc, doc.ID, req)
	if answer.FinishReason != "error" || answer.ErrorCode == "" || !strings.HasPrefix(answer.Content, "The flow starts by") {
		t.Errorf("stored %+v, want the partial text with finish error and a code", answer)
	}
}

// TestFailedTurn_RestartWithoutAFixStoresNothing: a turn the server's stop ended is re-sent by the client, so only
// one that changed the flow is stored; partial text alone is not.
func TestFailedTurn_RestartWithoutAFixStoresNothing(t *testing.T) {
	doc := fixTestDoc()
	svc := failedTurnService(t, "r-approvals-4-restart", &testutil.FakeProvider{}, doc)
	ctl := &streamCtl{done: false, errMsg: serverRestartingMsg, errCode: ChatErrServerRestarting}
	ctl.buffer.WriteString("Half an answer")
	req := models.ChatRequest{Provider: "r-approvals-4-restart", ConversationID: uuid.NewString(), AssistantMessageID: uuid.NewString()}
	if w := svc.persistAssistantTurn(context.Background(), doc, req, ctl, "", ""); w.Written {
		t.Error("a restarted turn with only partial text was stored; the client re-sends it")
	}
}

// TestConversationStore_RoundTripKeepsEveryField: a message stored through the backend (cloud) comes back with
// every field it was given — a field the storage type lacks is dropped silently on every write.
func TestConversationStore_RoundTripKeepsEveryField(t *testing.T) {
	var msg models.ChatMessage
	fillEveryField(reflect.ValueOf(&msg).Elem())
	msg.Timestamp = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	back := toModelMessages(toStorageMessages([]models.ChatMessage{msg}))
	if len(back) != 1 {
		t.Fatalf("round trip returned %d messages", len(back))
	}
	if !reflect.DeepEqual(back[0], msg) {
		t.Errorf("round trip lost fields:\n got  %+v\n want %+v", back[0], msg)
	}
}

// fillEveryField sets every settable field of v to a non-zero value (one element per slice, pointers allocated).
func fillEveryField(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x-" + v.Type().Name())
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(7)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
		fillEveryField(v.Elem())
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillEveryField(s.Index(0))
		v.Set(s)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				fillEveryField(v.Field(i))
			}
		}
	}
}

// TestBuildScopedMessages_FailedTurnsSendNoErrorLine: a stored failed turn is history the model reads; its partial
// text goes, its error line (the client's copy of the failure) does not, and one with nothing else is left out.
func TestBuildScopedMessages_FailedTurnsSendNoErrorLine(t *testing.T) {
	_, partial := failedTurnRecord("The flow starts by", ChatErrProviderUnavailable, "the AI provider is temporarily unavailable")
	_, bare := failedTurnRecord("", ChatErrProviderUnavailable, "the AI provider is temporarily unavailable")
	req := models.ChatRequest{UserMessage: "and now?", Messages: []models.ChatMessage{
		{Role: "user", Content: "explain"},
		{Role: "assistant", Content: partial, FinishReason: "error"},
		{Role: "user", Content: "again"},
		{Role: "assistant", Content: bare, FinishReason: "error"},
	}}
	msgs := buildScopedMessages("", req, "", "")
	var got []string
	for _, m := range msgs {
		got = append(got, m.Role+": "+m.Content)
	}
	want := []string{"user: explain", "assistant: The flow starts by", "user: again", "user: and now?"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("model history = %q, want %q", got, want)
	}
}
