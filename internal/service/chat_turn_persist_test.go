package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// savesCountingBackend is the fake backend with its conversation writes counted.
type savesCountingBackend struct {
	*testutil.FakeBackend
	saves atomic.Int32
}

func (b *savesCountingBackend) SaveConversation(ctx context.Context, flowID, scope string, messages []storageif.ChatMessage) error {
	b.saves.Add(1)
	return b.FakeBackend.SaveConversation(ctx, flowID, scope, messages)
}

func (b *savesCountingBackend) SaveConversationIf(ctx context.Context, flowID, scope string, expected int64, messages []storageif.ChatMessage) (int64, error) {
	b.saves.Add(1)
	return b.FakeBackend.SaveConversationIf(ctx, flowID, scope, expected, messages)
}

// A server-owned turn (the client sent no history) is two conversation
// writes — the question at stream start, the answer at stream end — and the
// stored answer carries the tool trail and finish reason the client used to
// add with a third, full-replace save of its own on done.
func TestServerOwnedTurn_TwoWritesAndTheTrailStored(t *testing.T) {
	fake := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{
			{ID: "t1", Name: "search_flow", Input: []byte(`{"query":"xavier"}`)},
			{ID: "t2", Name: "search_flow", Input: []byte(`{"query":"nothing-here"}`)},
		}, TokensIn: 10, TokensOut: 5},
		{Text: "Found Xavier.", TokensIn: 8, TokensOut: 3},
	}}
	ai.RegisterProvider("r4-tools-fake", func(string) ai.Provider { return fake })
	backend := &savesCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	svc := &ChatService{
		notifier:      &testutil.CountingNotifier{},
		configDir:     t.TempDir(),
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
		settings:      nilSettings{},
		chatCtxCache:  newChatContextCache(),
		backend:       backend,
		mode:          config.ModeCloud,
	}
	doc := toolLoopDoc()
	req := models.ChatRequest{
		Provider: "r4-tools-fake", UserMessage: "where is xavier?", UseTools: true,
		UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString(),
	}
	id, err := svc.StreamChatMessage(context.Background(), "test", doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.BeginStream(context.Background(), id)
	ctl := waitStreamFinished(t, svc, id)
	if !ctl.done || ctl.errMsg != "" {
		t.Fatalf("stream did not finish cleanly: done=%v err=%q", ctl.done, ctl.errMsg)
	}

	if n := backend.saves.Load(); n != 2 {
		t.Errorf("the turn wrote the conversation %d times, want 2 (question at start, answer at end)", n)
	}
	history, err := svc.GetConversation(context.Background(), doc.ID, req.ConversationKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Role != "user" || history[1].Role != "assistant" {
		t.Fatalf("stored %d messages (%+v), want the question and the answer", len(history), history)
	}
	if history[0].ID != req.UserMessageID || history[1].ID != req.AssistantMessageID {
		t.Errorf("stored ids %q / %q, want the client's %q / %q", history[0].ID, history[1].ID, req.UserMessageID, req.AssistantMessageID)
	}
	answer := history[1]
	if answer.Content != "Found Xavier." || answer.FinishReason != "stop" {
		t.Errorf("answer stored as %q / finish %q", answer.Content, answer.FinishReason)
	}
	if len(answer.ToolCalls) != 2 {
		t.Fatalf("stored %d tool calls, want 2: %+v", len(answer.ToolCalls), answer.ToolCalls)
	}
	for i, tc := range answer.ToolCalls {
		if tc.Name != "search_flow" || tc.Label == "" || tc.Summary == "" {
			t.Errorf("tool call %d stored as %+v", i, tc)
		}
	}
	if !answer.ToolCalls[0].Ok || !answer.ToolCalls[1].Ok {
		t.Errorf("tool calls not stored as ok: %+v", answer.ToolCalls)
	}
	if answer.TokensIn != 18 || answer.TokensOut != 8 {
		t.Errorf("usage stored as %d/%d, want 18/8", answer.TokensIn, answer.TokensOut)
	}
}

// The journal's events map onto the stored records exactly as the client
// pins them: tool results in order; a single fix's flat proposal as one
// item that takes the terminal decision; a batch's items resolved by rule
// id, unknown statuses clamped to "error"; decisions for unknown proposals
// and repeated proposals ignored.
func TestJournalTurnRecord(t *testing.T) {
	events := []journalEvent{
		{Type: "tool", Data: map[string]interface{}{"name": "search_flow", "label": "Searching flow"}},
		{Type: "tool_result", Data: map[string]interface{}{"name": "search_flow", "label": "Searching flow", "ok": true, "durationMs": int64(7), "summary": "3 matches"}},
		{Type: "tool_result", Data: map[string]interface{}{"name": "read_doc", "label": "Reading", "ok": false, "durationMs": float64(2), "summary": "error: no doc"}},
		{Type: "fix_proposal", Data: map[string]interface{}{"proposalId": "p1", "ruleId": "unhandled-error", "fixType": "wrap-error-handler", "blockId": "b1", "blockLabel": "Call API", "line": 3, "summary": "wrap"}},
		{Type: "fix_proposal", Data: map[string]interface{}{"proposalId": "p1", "ruleId": "dup", "summary": "replayed"}},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": "p1", "status": "applying"}},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": "ghost", "status": "applied"}},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": "p1", "status": "applied", "message": "verified gone"}},
		{Type: "fix_proposal", Data: map[string]interface{}{"proposalId": "p2", "batch": true, "count": 3, "items": []map[string]interface{}{
			{"ruleId": "r1", "fixType": "f1", "blockLabel": "B1", "line": 10, "summary": "s1"},
			{"ruleId": "r2", "fixType": "f2", "blockLabel": "B2", "line": 20, "summary": "s2"},
			{"ruleId": "r3", "fixType": "f3", "blockLabel": "B3", "line": 30, "summary": "s3"},
		}}},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": "p2", "status": "applied-unresolved", "message": "one left", "items": []map[string]interface{}{
			{"ruleId": "r1", "status": "applied"},
			{"ruleId": "r2", "status": "already-resolved", "message": "gone already"},
			{"ruleId": "r3", "status": "what-is-this"},
		}}},
		{Type: "fix_proposal", Data: map[string]interface{}{"proposalId": "p3", "ruleId": "r9", "summary": "declined one"}},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": "p3", "status": "declined"}},
		{Type: "fix_proposal", Data: map[string]interface{}{"proposalId": "p4", "ruleId": "r10", "summary": "still applying"}},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": "p4", "status": "applying"}},
	}
	calls, fixes := journalTurnRecord(events)
	wantCalls := []models.ToolCallRecord{
		{Name: "search_flow", Label: "Searching flow", Ok: true, DurationMs: 7, Summary: "3 matches"},
		{Name: "read_doc", Label: "Reading", Ok: false, DurationMs: 2, Summary: "error: no doc"},
	}
	if fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("tool calls = %+v, want %+v", calls, wantCalls)
	}
	if len(fixes) != 4 {
		t.Fatalf("got %d fix records, want 4: %+v", len(fixes), fixes)
	}
	single := fixes[0]
	if single.ProposalID != "p1" || single.RuleID != "unhandled-error" || single.FixType != "wrap-error-handler" ||
		single.BlockLabel != "Call API" || single.Line != 3 || single.Summary != "wrap" ||
		single.Status != "applied" || single.Message != "verified gone" {
		t.Errorf("single fix record = %+v", single)
	}
	if len(single.Items) != 1 || single.Items[0].Status != "applied" || single.Items[0].Message != "verified gone" || single.Items[0].RuleID != "unhandled-error" {
		t.Errorf("single fix item = %+v", single.Items)
	}
	batch := fixes[1]
	if batch.Status != "applied-unresolved" || batch.Message != "one left" || batch.RuleID != "r1" || batch.Line != 10 || len(batch.Items) != 3 {
		t.Errorf("batch record = %+v", batch)
	}
	if got := strings.Join([]string{batch.Items[0].Status, batch.Items[1].Status, batch.Items[1].Message, batch.Items[2].Status}, "|"); got != "applied|already-resolved|gone already|error" {
		t.Errorf("batch items = %q", got)
	}
	if fixes[2].Status != "declined" || fixes[2].Items[0].Status != "error" {
		t.Errorf("declined record = %+v", fixes[2])
	}
	if fixes[3].Status != "applying" || fixes[3].Items[0].Status != "pending" {
		t.Errorf("a non-terminal decision must leave the item pending: %+v", fixes[3])
	}

	// The same events after a JSON round trip (float64 numbers, []interface{} items).
	raw, _ := json.Marshal(events)
	var decoded []journalEvent
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	calls2, fixes2 := journalTurnRecord(decoded)
	if fmt.Sprint(calls2) != fmt.Sprint(calls) || fmt.Sprint(fixes2) != fmt.Sprint(fixes) {
		t.Error("JSON-decoded events produce different records")
	}
	if c, f := journalTurnRecord(nil); c != nil || f != nil {
		t.Error("no events must produce nil records (omitted from the stored message)")
	}
}

// r4Ctl is a finished stream control with answer and journal events.
func r4Ctl(answer string, events ...journalEvent) *streamCtl {
	ctl := &streamCtl{cancel: func() {}, started: make(chan struct{}), done: true}
	ctl.buffer.WriteString(answer)
	for _, ev := range events {
		ctl.recordJournal(ev.Type, ev.Data)
	}
	return ctl
}

// The answer is stored right after its own question: a queued follow-up's
// question can be stored between the done event and the answer write, and
// the client no longer puts the thread back in order with a save of its own.
// A blank answer is stored when tools ran, dropped when nothing happened.
func TestPersistAssistantTurn_InsertsAfterItsQuestion(t *testing.T) {
	svc := &ChatService{configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	q1, q2, a1 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	req := models.ChatRequest{Provider: "fake", UserMessage: "first", UserMessageID: q1, AssistantMessageID: a1}
	seed := []models.ChatMessage{
		{ID: q1, Role: "user", Content: "first"},
		{ID: q2, Role: "user", Content: "second (queued, landed first)"},
	}
	if err := svc.SaveConversation(context.Background(), doc.ID, req.ConversationKey(), seed); err != nil {
		t.Fatal(err)
	}
	svc.persistAssistantTurn(context.Background(), doc, req, r4Ctl("answer one"), "fake-model", "")
	history, err := svc.GetConversation(context.Background(), doc.ID, req.ConversationKey())
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, m := range history {
		order = append(order, m.Role+":"+m.Content)
	}
	if got := strings.Join(order, " | "); got != "user:first | assistant:answer one | user:second (queued, landed first)" {
		t.Errorf("stored order = %s", got)
	}
	if history[1].ID != a1 || history[1].FinishReason != "stop" {
		t.Errorf("answer stored as %+v", history[1])
	}

	// Blank answer, tools ran: stored with its trail. Blank, nothing ran: not stored.
	req2 := models.ChatRequest{Provider: "fake", UserMessage: "second", UserMessageID: q2, AssistantMessageID: uuid.NewString()}
	svc.persistAssistantTurn(context.Background(), doc, req2, r4Ctl("   "), "fake-model", "")
	if history, _ = svc.GetConversation(context.Background(), doc.ID, req.ConversationKey()); len(history) != 3 {
		t.Fatalf("a blank answer without tools was stored: %d messages", len(history))
	}
	toolOnly := r4Ctl("", journalEvent{Type: "tool_result", Data: map[string]interface{}{"name": "search_flow", "label": "Searching flow", "ok": true, "durationMs": int64(1), "summary": "none"}})
	svc.persistAssistantTurn(context.Background(), doc, req2, toolOnly, "fake-model", "")
	history, _ = svc.GetConversation(context.Background(), doc.ID, req.ConversationKey())
	if len(history) != 4 || len(history[3].ToolCalls) != 1 || history[3].Role != "assistant" {
		t.Fatalf("a blank answer with a tool trail must be stored with it: %+v", history)
	}
}

// The 50-message cap applies on its own: a history within the byte budget
// used to come back untrimmed however many messages it had.
func TestEvictConvMessages_CountCapApplies(t *testing.T) {
	mk := func(n int) []models.ChatMessage {
		msgs := make([]models.ChatMessage, n)
		for i := range msgs {
			msgs[i] = models.ChatMessage{ID: fmt.Sprintf("m%d", i), Role: "user", Content: "short"}
		}
		return msgs
	}
	if out := evictConvMessages(mk(maxConvMessages)); len(out) != maxConvMessages {
		t.Errorf("a history at the cap was trimmed to %d", len(out))
	}
	out := evictConvMessages(mk(maxConvMessages + 10))
	if len(out) != maxConvMessages {
		t.Fatalf("maxConvMessages+10 small messages kept as %d, want %d", len(out), maxConvMessages)
	}
	if out[0].ID != "m10" || out[len(out)-1].ID != fmt.Sprintf("m%d", maxConvMessages+9) {
		t.Errorf("the oldest must go: kept %s … %s", out[0].ID, out[len(out)-1].ID)
	}
	// Trimmed in pairs (a question and its answer together), never below the floor.
	if out := evictConvMessages(mk(maxConvMessages + 1)); len(out) != maxConvMessages-1 {
		t.Errorf("cap+1 messages kept as %d, want cap-1 (pairs)", len(out))
	}
	if out := evictConvMessages(mk(maxConvMessages + 2)); len(out) < minRetainedConvMsgs {
		t.Errorf("trimmed below the floor: %d", len(out))
	}
}
