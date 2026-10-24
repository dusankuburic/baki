package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// noCostKeys fails when raw carries a cost or rate key: Baki records token counts only.
func noCostKeys(t *testing.T, where string, raw []byte) {
	t.Helper()
	for _, key := range []string{"CostPerM", "estimatedCost", `"cost"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("%s carries %s: %s", where, key, raw)
		}
	}
}

// TestConversation_KeepsServedModel: a saved conversation round-trips an assistant message's served model on both
// persistence paths (the local file and the cloud backend through the models ↔ storage bridge), under the JSON name
// the frontend's lib/chatMessage.ts reads, omitted when not reported, and carries no cost.
func TestConversation_KeepsServedModel(t *testing.T) {
	ts := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	msgs := []models.ChatMessage{
		{ID: "1", Role: "user", Content: "hi", Timestamp: ts},
		{
			ID: "2", Role: "assistant", Content: "answer", Timestamp: ts, Provider: "azure-foundry", Model: "my-deployment",
			TokensIn: 7, TokensOut: 3, ServedModel: "gpt-4o-2024-11-20",
		},
	}
	check := func(t *testing.T, path string, got []models.ChatMessage) {
		t.Helper()
		if len(got) != 2 {
			t.Fatalf("[%s] %d messages, want 2", path, len(got))
		}
		if m := got[1]; m.ServedModel != "gpt-4o-2024-11-20" || m.Model != "my-deployment" || m.TokensIn != 7 {
			t.Errorf("[%s] assistant message lost the served model: %+v", path, m)
		}
		if m := got[0]; m.ServedModel != "" {
			t.Errorf("[%s] user message gained a served model: %+v", path, m)
		}
	}
	doc := &models.FlowDocument{ID: "flow-1"}

	t.Run("filesystem", func(t *testing.T) {
		svc := &ChatService{configDir: t.TempDir()}
		if err := svc.SaveConversation(context.Background(), doc.ID, "flow", msgs); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		got, err := svc.GetConversation(context.Background(), doc.ID, "flow")
		if err != nil {
			t.Fatalf("GetConversation: %v", err)
		}
		check(t, "filesystem", got)
	})
	t.Run("backend", func(t *testing.T) {
		store := &testutil.FakeBackend{}
		svc := &ChatService{configDir: t.TempDir(), mode: config.ModeCloud, backend: store}
		if err := svc.SaveConversation(context.Background(), doc.ID, "flow", msgs); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		stored, err := store.LoadConversation(context.Background(), doc.ID, "flow")
		if err != nil || len(stored) != 2 {
			t.Fatalf("stored = %v, %v", stored, err)
		}
		raw, _ := json.Marshal(stored)
		if !strings.Contains(string(raw), `"servedModel":"gpt-4o-2024-11-20"`) {
			t.Errorf("stored JSON lacks the served model: %s", raw)
		}
		noCostKeys(t, "stored JSON", raw)
		got, err := svc.GetConversation(context.Background(), doc.ID, "flow")
		if err != nil {
			t.Fatalf("GetConversation: %v", err)
		}
		check(t, "backend", got)
	})
	t.Run("json names", func(t *testing.T) {
		raw, _ := json.Marshal(msgs[1])
		if !strings.Contains(string(raw), `"servedModel":"gpt-4o-2024-11-20"`) {
			t.Errorf("models.ChatMessage JSON lacks the served model: %s", raw)
		}
		noCostKeys(t, "models.ChatMessage JSON", raw)
		if raw, _ := json.Marshal(msgs[0]); strings.Contains(string(raw), "servedModel") {
			t.Errorf("an unreported served model must be omitted: %s", raw)
		}
		// A message an older client saved with rates still decodes; the rates are dropped.
		var back models.ChatMessage
		if err := json.Unmarshal([]byte(`{"id":"x","role":"assistant","content":"","timestamp":"2026-09-28T12:00:00Z","servedModel":"m","inputCostPerM":1.25,"outputCostPerM":5}`), &back); err != nil {
			t.Fatal(err)
		}
		if back.ServedModel != "m" {
			t.Errorf("a client-saved message decoded as %+v", back)
		}
	})
}

// TestResumeAndBegin_CarryServedModel pins the resume side: a finished stream's served model (from its done event)
// reaches a client that missed that event through ResumeStream, BeginStream and the cross-replica snapshot, and the
// answer the server persists for a disconnected client carries it too. None of them carries a cost.
func TestResumeAndBegin_CarryServedModel(t *testing.T) {
	p := &servedFakeProvider{
		FakeProvider: &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 7, TokensOut: 3}}},
		served:       "gpt-4o-2024-11-20",
	}
	ai.RegisterProvider("served-fake", func(string) ai.Provider { return p })
	svc := &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
	}
	doc := &models.FlowDocument{ID: "f-served"}
	req := models.ChatRequest{Provider: "served-fake", Model: "dep", UserMessage: "hi", ClientStreamID: "12345678-1234-5678-1234-56781234567c"}
	id, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	ctl := waitStreamFinished(t, svc, id)

	served := func(where, got string) {
		t.Helper()
		if got != "gpt-4o-2024-11-20" {
			t.Errorf("%s: served=%q, want gpt-4o-2024-11-20", where, got)
		}
	}
	snap := ctl.snapshot()
	served("snapshot", snap.ServedModel)
	res, err := svc.ResumeStream(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("ResumeStream: %v", err)
	}
	served("ResumeStream", res.ServedModel)
	if begin := svc.BeginStream(context.Background(), id); begin == nil {
		t.Error("BeginStream on a finished stream returned nil, want its final state")
	} else {
		served("BeginStream", begin.ServedModel)
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	noCostKeys(t, "snapshot JSON", raw)
	var back resumeSnapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	fromBackplane := back.result("tail")
	served("backplane result", fromBackplane.ServedModel)
	if fromBackplane.Text != "tail" || !fromBackplane.Done || fromBackplane.TokensIn != 7 {
		t.Errorf("backplane result = %+v", fromBackplane)
	}
	wire, _ := json.Marshal(res)
	noCostKeys(t, "ResumeResult JSON", wire)

	history, err := svc.GetConversation(context.Background(), doc.ID, req.ConversationKey())
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var answer *models.ChatMessage
	for i := range history {
		if history[i].Role == "assistant" {
			answer = &history[i]
		}
	}
	if answer == nil {
		t.Fatalf("no persisted answer in %+v", history)
	}
	served("persisted answer", answer.ServedModel)
	if answer.TokensIn != 7 || answer.TokensOut != 3 {
		t.Errorf("persisted answer usage = %d/%d, want 7/3", answer.TokensIn, answer.TokensOut)
	}
}
