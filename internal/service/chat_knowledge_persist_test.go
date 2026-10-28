package service

// R-knowledge-1: the knowledge notice a turn showed is stored with its answer, so a reloaded conversation still
// says the organisation's guidelines were not consulted.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	tu "pad-analyzer/internal/testutil"
	"pad-core/models"
)

func TestPersistAssistantTurn_StoresTheKnowledgeNotice(t *testing.T) {
	ctx := context.Background()
	svc := cloudChat(tu.NewFakeBackend())
	doc := &models.FlowDocument{ID: "flow-knowledge-notice"}
	req := models.ChatRequest{ConversationID: uuid.NewString(), AssistantMessageID: uuid.NewString()}
	ctl := finishedAnswer("an answer without the guidelines")
	ctl.mu.Lock()
	ctl.recordJournal("notice", map[string]interface{}{"kind": noticeKnowledge, "status": "identity_mismatch"})
	ctl.mu.Unlock()

	svc.persistAssistantTurn(ctx, doc, req, ctl, "fake-model", "")

	stored, err := svc.GetConversation(ctx, doc.ID, req.ConversationKey())
	if err != nil {
		t.Fatal(err)
	}
	if got := findMessage(t, "the store", stored, req.AssistantMessageID).Knowledge; got != "identity_mismatch" {
		t.Errorf("stored knowledge %q, want identity_mismatch", got)
	}
}
