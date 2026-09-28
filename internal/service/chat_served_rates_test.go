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

// TestConversation_KeepsServedModelAndRates pins R-3: a saved conversation
// round-trips an assistant message's served model and billed rates on both
// persistence paths (the local file and the cloud backend through the
// models ↔ storage bridge), under the JSON names the frontend's
// lib/chatMessage.ts reads, omitted when not reported.
func TestConversation_KeepsServedModelAndRates(t *testing.T) {
	ts := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	msgs := []models.ChatMessage{
		{ID: "1", Role: "user", Content: "hi", Timestamp: ts},
		{
			ID: "2", Role: "assistant", Content: "answer", Timestamp: ts, Provider: "azure-foundry", Model: "my-deployment",
			TokensIn: 7, TokensOut: 3, ServedModel: "gpt-4o-2024-11-20", InputCostPerM: 2.5, OutputCostPerM: 10,
		},
	}
	check := func(t *testing.T, path string, got []models.ChatMessage) {
		t.Helper()
		if len(got) != 2 {
			t.Fatalf("[%s] %d messages, want 2", path, len(got))
		}
		if m := got[1]; m.ServedModel != "gpt-4o-2024-11-20" || m.InputCostPerM != 2.5 || m.OutputCostPerM != 10 || m.Model != "my-deployment" {
			t.Errorf("[%s] assistant message lost the served model / rates: %+v", path, m)
		}
		if m := got[0]; m.ServedModel != "" || m.InputCostPerM != 0 || m.OutputCostPerM != 0 {
			t.Errorf("[%s] user message gained fields: %+v", path, m)
		}
	}
	doc := &models.FlowDocument{ID: "flow-1"}

	t.Run("filesystem", func(t *testing.T) {
		svc := &ChatService{configDir: t.TempDir()}
		if err := svc.SaveConversation(context.Background(), doc, "flow", msgs); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		got, err := svc.GetConversation(context.Background(), doc, "flow")
		if err != nil {
			t.Fatalf("GetConversation: %v", err)
		}
		check(t, "filesystem", got)
	})
	t.Run("backend", func(t *testing.T) {
		store := &testutil.FakeBackend{}
		svc := &ChatService{configDir: t.TempDir(), mode: config.ModeCloud, backend: store}
		if err := svc.SaveConversation(context.Background(), doc, "flow", msgs); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		// Through the storage type's JSON, as Postgres stores it (messages
		// JSONB — no column, no migration).
		stored, err := store.LoadConversation(context.Background(), doc.ID, "flow")
		if err != nil || len(stored) != 2 {
			t.Fatalf("stored = %v, %v", stored, err)
		}
		raw, _ := json.Marshal(stored)
		for _, want := range []string{`"servedModel":"gpt-4o-2024-11-20"`, `"inputCostPerM":2.5`, `"outputCostPerM":10`} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("stored JSON lacks %s: %s", want, raw)
			}
		}
		got, err := svc.GetConversation(context.Background(), doc, "flow")
		if err != nil {
			t.Fatalf("GetConversation: %v", err)
		}
		check(t, "backend", got)
	})
	t.Run("json names", func(t *testing.T) {
		raw, _ := json.Marshal(msgs[1])
		for _, want := range []string{`"servedModel":"gpt-4o-2024-11-20"`, `"inputCostPerM":2.5`, `"outputCostPerM":10`} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("models.ChatMessage JSON lacks %s: %s", want, raw)
			}
		}
		if raw, _ := json.Marshal(msgs[0]); strings.Contains(string(raw), "servedModel") || strings.Contains(string(raw), "CostPerM") {
			t.Errorf("unreported fields must be omitted: %s", raw)
		}
		var back models.ChatMessage
		if err := json.Unmarshal([]byte(`{"id":"x","role":"assistant","content":"","timestamp":"2026-09-28T12:00:00Z","servedModel":"m","inputCostPerM":1.25,"outputCostPerM":5}`), &back); err != nil {
			t.Fatal(err)
		}
		if back.ServedModel != "m" || back.InputCostPerM != 1.25 || back.OutputCostPerM != 5 {
			t.Errorf("a client-saved message decoded as %+v", back)
		}
	})
}

// TestResumeAndBegin_CarryBilledRates pins the Go side of R-3 for resume: a
// finished stream's billed rates (from its done event) reach a client that
// missed that event through ResumeStream, BeginStream and the cross-replica
// snapshot, next to the served model; and the answer the server persists
// for a disconnected client carries them too.
func TestResumeAndBegin_CarryBilledRates(t *testing.T) {
	p := &servedFakeProvider{
		FakeProvider: &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 7, TokensOut: 3}}},
		served:       "gpt-4o-2024-11-20", rate: ai.Pricing{InputCostPerM: 2.5, OutputCostPerM: 10},
	}
	ai.RegisterProvider("served-fake", func(string) ai.Provider { return p })
	svc := &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
	}
	doc := &models.FlowDocument{ID: "f-rates"}
	req := models.ChatRequest{Provider: "served-fake", Model: "dep", UserMessage: "hi", ClientStreamID: "12345678-1234-5678-1234-56781234567c"}
	id, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	ctl := waitStreamFinished(t, svc, id)

	rated := func(where string, served string, in, out float64) {
		t.Helper()
		if served != "gpt-4o-2024-11-20" || in != 2.5 || out != 10 {
			t.Errorf("%s: served=%q rates=%v/%v, want gpt-4o-2024-11-20 at 2.5/10", where, served, in, out)
		}
	}
	snap := ctl.snapshot()
	rated("snapshot", snap.ServedModel, snap.InputCostPerM, snap.OutputCostPerM)
	res, err := svc.ResumeStream(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("ResumeStream: %v", err)
	}
	rated("ResumeStream", res.ServedModel, res.InputCostPerM, res.OutputCostPerM)
	if begin := svc.BeginStream(context.Background(), id); begin == nil {
		t.Error("BeginStream on a finished stream returned nil, want its final state")
	} else {
		rated("BeginStream", begin.ServedModel, begin.InputCostPerM, begin.OutputCostPerM)
	}

	// Cross-replica: the snapshot's JSON and the result built from it.
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var back resumeSnapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	fromBackplane := back.result("tail")
	rated("backplane result", fromBackplane.ServedModel, fromBackplane.InputCostPerM, fromBackplane.OutputCostPerM)
	if fromBackplane.Text != "tail" || !fromBackplane.Done || fromBackplane.TokensIn != 7 {
		t.Errorf("backplane result = %+v", fromBackplane)
	}
	wire, _ := json.Marshal(res)
	if !strings.Contains(string(wire), `"inputCostPerM":2.5`) || !strings.Contains(string(wire), `"outputCostPerM":10`) {
		t.Errorf("ResumeResult JSON = %s, want inputCostPerM / outputCostPerM", wire)
	}
	if wire, _ := json.Marshal(ResumeResult{}); strings.Contains(string(wire), "CostPerM") {
		t.Errorf("unreported rates must be omitted: %s", wire)
	}

	// The server-side persist of the finished answer.
	history, err := svc.GetConversation(context.Background(), doc, req.ConversationKey())
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
	rated("persisted answer", answer.ServedModel, answer.InputCostPerM, answer.OutputCostPerM)
	if answer.TokensIn != 7 || answer.TokensOut != 3 {
		t.Errorf("persisted answer usage = %d/%d, want 7/3", answer.TokensIn, answer.TokensOut)
	}
}

// TestNoteDoneRates_IgnoresMissingAndForeignValues: a done payload without
// rates (a provider that cannot price the turn) or with a non-numeric value
// leaves the rates unreported.
func TestNoteDoneRates_IgnoresMissingAndForeignValues(t *testing.T) {
	c := &streamCtl{}
	c.recordJournal("done", map[string]interface{}{"tokensIn": 1})
	c.recordJournal("done", map[string]interface{}{"inputCostPerM": "2.5"})
	if c.inputCostPerM != 0 || c.outputCostPerM != 0 {
		t.Errorf("rates = %v/%v, want unreported", c.inputCostPerM, c.outputCostPerM)
	}
	c.recordJournal("chunk", map[string]interface{}{"inputCostPerM": 9.0})
	if c.inputCostPerM != 0 {
		t.Error("a non-done event set the rate")
	}
	c.recordJournal("done", map[string]interface{}{"inputCostPerM": 0.15, "outputCostPerM": 0.6})
	if c.inputCostPerM != 0.15 || c.outputCostPerM != 0.6 {
		t.Errorf("rates = %v/%v, want 0.15/0.6", c.inputCostPerM, c.outputCostPerM)
	}
}
