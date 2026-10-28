package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/audit"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// Fixes of the step 4 review round.

// TestHistoryContent_AnyErrorLineIsDropped: the client words its error bubbles its own way (a provider's title, the
// restart copy); what follows the last separator of a failed message is the failure, whatever its wording, and a
// failed message without one is all failure.
func TestHistoryContent_AnyErrorLineIsDropped(t *testing.T) {
	for _, tc := range []struct{ content, want string }{
		{"The flow starts by\n\n---\n**Claude is unavailable** — try again later", "The flow starts by"},
		{"*Error: The server restarted before this answer finished — use Retry to send it again.*", ""},
		{"A rule\n\n---\nB rule\n\n---\n*Error: boom*", "A rule\n\n---\nB rule"},
	} {
		if got := historyContent(models.ChatMessage{Role: "assistant", FinishReason: "error", Content: tc.content}); got != tc.want {
			t.Errorf("historyContent(%q) = %q, want %q", tc.content, got, tc.want)
		}
	}
	if got := historyContent(models.ChatMessage{Role: "assistant", FinishReason: "stop", Content: "a\n\n---\nb"}); got != "a\n\n---\nb" {
		t.Errorf("a finished answer was cut: %q", got)
	}
}

// TestPersistAssistantTurn_SettlesAStoredApplyingCard: the client stored the turn's message at a stop or a close with
// its card still applying; the server's end-of-turn write knows the outcome from the journal and settles the card,
// instead of skipping the message as already stored.
func TestPersistAssistantTurn_SettlesAStoredApplyingCard(t *testing.T) {
	doc := fixTestDoc()
	svc := failedTurnService(t, "review-settle", nil, doc)
	answerID := uuid.NewString()
	req := models.ChatRequest{Provider: "review-settle", ConversationID: uuid.NewString(), AssistantMessageID: answerID}
	stored := []models.ChatMessage{
		{ID: uuid.NewString(), Role: "user", Content: "fix it"},
		{ID: answerID, Role: "assistant", Content: "Applying.", FinishReason: "interrupted", FixProposals: []models.FixProposalSnapshot{{
			ProposalID: "p1", RuleID: "unhandled-error", Status: "applying",
			Items: []models.FixItemSnapshot{{ID: "b1:unhandled-error", RuleID: "unhandled-error", Status: "pending"}},
		}}},
	}
	if _, err := svc.saveConversation(context.Background(), doc.ID, req.ConversationKey(), stored); err != nil {
		t.Fatal(err)
	}
	ctl := &streamCtl{errMsg: "response stopped", errCode: ChatErrStopped}
	ctl.buffer.WriteString("Applying.")
	ctl.journal = []journalEvent{
		{Type: "fix_proposal", Data: map[string]interface{}{"proposalId": "p1", "ruleId": "unhandled-error", "items": []map[string]interface{}{{"id": "b1:unhandled-error", "ruleId": "unhandled-error"}}}},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": "p1", "status": "applying"}},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": "p1", "status": "applied", "message": "verified"}},
	}
	svc.persistAssistantTurn(context.Background(), doc, req, ctl, "", "")

	answer := storedAnswer(t, svc, doc.ID, req)
	if len(answer.FixProposals) != 1 || answer.FixProposals[0].Status != "applied" || answer.FixProposals[0].Items[0].Status != "applied" {
		t.Errorf("stored card = %+v, want it settled applied", answer.FixProposals)
	}
	if answer.Content != "Applying." || answer.FinishReason != "interrupted" {
		t.Errorf("the client's message was rewritten beyond its card: %+v", answer)
	}
}

// TestChatFix_AuditRecordsOnlyWrites: a fix that wrote nothing (the finding was already gone) leaves the decision
// row but no flow.fix_apply row.
func TestChatFix_AuditRecordsOnlyWrites(t *testing.T) {
	doc := fixTestDoc()
	svc, ldp := newReassocService(doc, func(context.Context, *models.FlowDocument, string, string, string, string, string) (*models.FlowDocument, error) {
		return nil, errors.New("the hook must not run")
	})
	ch := make(chan *storageif.AuditEvent, 8)
	svc.audit = audit.NewCapture(ch)
	prop := proposalFor(t, svc, doc, "p-gone", "b1")
	approveWhenShown(svc, prop.ProposalID, func() { ldp.SetCurrentDoc(commentDoc(doc.ID)) })
	if _, err := svc.newFixApplier(doc, "stream-1", (&fixEventRecorder{}).emit, nil, nil).ApplyFixWithApproval(context.Background(), prop); err != nil {
		t.Fatal(err)
	}
	for _, ev := range drainAudit(ch) {
		if ev.Action == audit.ActionFlowFixApply {
			t.Errorf("a fix that wrote nothing was audited as a write: %+v", ev.Meta)
		}
	}
}

// TestApplyFixes_FailureBeforeTheLoopReportsEveryItem: a batch that cannot start (the edit check fails) still
// reports each kept item, with the reason, next to the deselected one.
func TestApplyFixes_FailureBeforeTheLoopReportsEveryItem(t *testing.T) {
	doc := threeCallDoc("flow-1")
	svc, _ := newFixTestService(doc, nil)
	svc.analysisCache = nil // "analysis not available — could not prepare the batch"
	rec := &decisionRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	props := []ai.FixProposal{{ProposalID: "p1", RuleID: "unhandled-error", BlockID: "b1"}, {ProposalID: "p2", RuleID: "unhandled-error", BlockID: "b2"}, {ProposalID: "p3", RuleID: "unhandled-error", BlockID: "b3"}}
	approveBatchWhenShownExcluding(svc, []int{1})
	if _, err := ap.ApplyFixesWithApproval(context.Background(), props); err != nil {
		t.Fatal(err)
	}
	items, _ := rec.last()["items"].([]map[string]interface{})
	want := []string{"error", "skipped", "error"}
	if len(items) != 3 {
		t.Fatalf("decision items = %v, want one per proposal", items)
	}
	for i, w := range want {
		if items[i]["status"] != w {
			t.Errorf("item %d = %v, want %s", i, items[i], w)
		}
	}
	if m, _ := items[0]["message"].(string); !strings.Contains(m, "analysis not available") {
		t.Errorf("item 0 message %q, want the batch's reason", m)
	}
}

func approveBatchWhenShownExcluding(svc *ChatService, excluded []int) {
	go func() {
		for i := 0; i < 1500; i++ {
			done := false
			svc.pendingFixDecisions.Range(func(k, _ any) bool {
				_ = svc.ResolveFixDecision("stream-1", k.(string), true, excluded)
				done = true
				return false
			})
			if done {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
}
