package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// Before each provider call the server drops the oldest turns until the prompt fits the window, and said
// nothing (R-streaming-7): the assistant silently forgot what was said ten turns ago. The turn now emits one
// journaled notice {kind: history_truncated, dropped, keptFrom} before the first chunk, the resume journal
// replays it, and the stored answer records how many turns were left out.
func TestStreamChatMessage_AnnouncesDroppedHistory(t *testing.T) {
	p := &testutil.FakeProvider{Ctx: 2000, RequireScrubbed: t, Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 3, TokensOut: 2}}}
	svc := newCtxBlockChatService(t, "history-notice", p)
	doc := parseCtxBlockDoc(t, ctxBlockSrc)
	ctx := context.Background()

	var history []models.ChatMessage
	for i := 0; i < 12; i++ {
		history = append(history,
			models.ChatMessage{ID: uuid.NewString(), Role: "user", Content: fmt.Sprintf("question %d: %s", i, strings.Repeat("why does the flow fail here ", 16))},
			models.ChatMessage{ID: uuid.NewString(), Role: "assistant", Content: fmt.Sprintf("answer %d: %s", i, strings.Repeat("because the loop never exits ", 16))},
		)
	}
	req := models.ChatRequest{Provider: "history-notice", Model: "fake-model", UserMessage: "and now?", ConversationID: uuid.NewString(), ClientStreamID: uuid.NewString()}
	if _, err := svc.saveConversation(ctx, doc.ID, req.ConversationKey(), history); err != nil {
		t.Fatal(err)
	}

	id, err := svc.StreamChatMessage(ctx, "u1", doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	waitStreamFinished(t, svc, id)

	types, payloads := eventTypes(svc.notifier.(*testutil.CountingNotifier))
	notices, firstNotice, firstChunk := 0, -1, -1
	dropped := 0
	for i, typ := range types {
		switch {
		case typ == "notice" && payloads[i]["kind"] == "history_truncated":
			notices++
			if firstNotice < 0 {
				firstNotice = i
			}
			dropped, _ = payloads[i]["dropped"].(int)
			if kept, _ := payloads[i]["keptFrom"].(int); kept != dropped {
				t.Errorf("keptFrom %v, want the dropped count %d on the first truncation", payloads[i]["keptFrom"], dropped)
			}
		case typ == "chunk" && firstChunk < 0:
			firstChunk = i
		}
	}
	if types[len(types)-1] != "done" {
		t.Fatalf("the turn did not finish with done: %v", types)
	}
	if notices != 1 || dropped <= 0 {
		t.Fatalf("history_truncated notices = %d (dropped %d), want exactly one with dropped > 0; events: %v", notices, dropped, types)
	}
	if firstChunk >= 0 && firstNotice > firstChunk {
		t.Errorf("the notice came after the first chunk (notice %d, chunk %d)", firstNotice, firstChunk)
	}

	reqs := p.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d provider requests", len(reqs))
	}
	if sent := len(reqs[0].Messages) - 1; sent != len(history)-dropped {
		t.Errorf("the provider got %d history messages, the stored %d less the %d announced", sent, len(history), dropped)
	}

	res, err := svc.ResumeStream(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	replayed := 0
	for _, ev := range res.Events {
		if ev.Type == "notice" && ev.Data["kind"] == "history_truncated" {
			replayed++
		}
	}
	if replayed != 1 {
		t.Errorf("the resume journal replays %d history_truncated notices, want 1", replayed)
	}

	stored, err := svc.GetConversation(ctx, doc.ID, req.ConversationKey())
	if err != nil {
		t.Fatal(err)
	}
	last := stored[len(stored)-1]
	if last.Role != "assistant" || last.Truncated != dropped {
		t.Errorf("the stored answer records truncated=%d (role %s), want %d", last.Truncated, last.Role, dropped)
	}
}

// A thread that fits emits no notice.
func TestStreamChatMessage_NoNoticeWhenTheHistoryFits(t *testing.T) {
	p := &testutil.FakeProvider{RequireScrubbed: t, Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 3, TokensOut: 2}}}
	svc := newCtxBlockChatService(t, "history-fits", p)
	doc := parseCtxBlockDoc(t, ctxBlockSrc)
	req := models.ChatRequest{Provider: "history-fits", Model: "fake-model", UserMessage: "hi", ClientStreamID: uuid.NewString(),
		Messages: []models.ChatMessage{{ID: "h1", Role: "user", Content: "earlier"}, {ID: "h2", Role: "assistant", Content: "yes"}}}
	id, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	waitStreamFinished(t, svc, id)
	types, _ := eventTypes(svc.notifier.(*testutil.CountingNotifier))
	for _, typ := range types {
		if typ == "notice" {
			t.Errorf("a fitting thread emitted a notice: %v", types)
		}
	}
}
