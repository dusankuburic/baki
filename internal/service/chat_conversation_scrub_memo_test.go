package service

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	tu "pad-analyzer/internal/testutil"
	"pad-core/ai/scrubber"
	"pad-core/models"
)

// Every write of a conversation runs the store scrub over the whole list: twice a turn (the question at stream
// start, under the thread's mutex and before the first token; the answer at stream end) and on every client save.
// Unmemoised, the secret regexes cost ~0.4 s per write on an anchor-bearing history near the 1 MiB cap, for the
// life of the thread. The write path therefore scrubs through the history memo (historyScrubs) under the caller's
// scope: a stored message is scrubbed once per process, and the turn's request build reuses the write's result.

// countingScrubMemo swaps historyScrubs for a memo that counts the scrubs of texts carrying nonce (a late stream of
// another test must not count) until the test ends.
func countingScrubMemo(t *testing.T, nonce string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	memo := newScrubMemo(1024, 8<<20)
	memo.scrub = func(s string) string {
		if strings.Contains(s, nonce) {
			calls.Add(1)
		}
		return scrubber.ScrubText(s)
	}
	orig := historyScrubs
	historyScrubs = memo
	t.Cleanup(func() { historyScrubs = orig })
	return &calls
}

// anchoredProse is prose that wakes the secret regexes (URLs, the words token and password) but holds no secret:
// what a long support thread looks like to the scrubber. About 100 bytes per repeat.
func anchoredProse(repeats int) string {
	return strings.Repeat("the token is rotated by the vault at https://vault.example.com/v1 and the password policy applies; ", repeats)
}

// anchoredHistory is n stored messages of anchored prose, each long enough for the memo and carrying nonce.
func anchoredHistory(n, repeats int, nonce string) []models.ChatMessage {
	msgs := make([]models.ChatMessage, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, models.ChatMessage{ID: uuid.NewString(), Role: role, Content: nonce + " " + anchoredProse(repeats) + strconv.Itoa(i)})
	}
	return msgs
}

// A write scrubs every message once through the memo; the next write of the same thread hits on every stored copy
// (a masked copy is new text once), the turn's request build under the same scope reuses the write's entries, and
// another caller's entries are their own (historyScrubSpec keys per caller).
func TestNormalizeConversation_ScrubsThroughTheHistoryMemo(t *testing.T) {
	nonce := "n-" + uuid.NewString()
	calls := countingScrubMemo(t, nonce)
	const secret = "hunter2hunter2"
	history := append(anchoredHistory(6, 3, nonce), models.ChatMessage{
		ID: "secret", Role: "user",
		Content: nonce + " " + strings.Repeat("the connection string it uses is ", 8) + "Server=db;User=sa;Password=" + secret + "; and it fails",
	})

	stored := normalizeConversation("u1", history)
	if n := calls.Load(); n != 7 {
		t.Fatalf("the first write scrubbed %d texts through the memo, want 7 (every message, once)", n)
	}
	assertMasked(t, "the stored secret", stored[6].Content, secret)

	again := normalizeConversation("u1", stored)
	if n := calls.Load() - 7; n != 1 {
		t.Errorf("the second write scrubbed %d texts, want 1 (the masked copy, seen for the first time)", n)
	}
	if !reflect.DeepEqual(stored, again) {
		t.Errorf("the second write changed the stored list:\n first: %+v\nsecond: %+v", stored, again)
	}
	normalizeConversation("u1", again)
	if n := calls.Load() - 8; n != 0 {
		t.Errorf("the third write scrubbed %d texts, want 0 (every stored copy memoised)", n)
	}

	req := models.ChatRequest{Messages: again, UserMessage: nonce + " one more question about the token: " + anchoredProse(3)}
	buildScopedMessages("u1", req, "", "")
	if n := calls.Load() - 8; n != 1 {
		t.Errorf("the request build scrubbed %d texts, want 1 (the new question only: the history reuses the write's entries)", n)
	}

	normalizeConversation("u2", again)
	if n := calls.Load() - 9; n != 7 {
		t.Errorf("another caller's write scrubbed %d texts, want 7 (entries are per caller)", n)
	}
}

// A turn on a thread whose history the memo holds scrubs two texts: the question once (the write at stream start;
// the request build reuses it) and the answer once (the write at stream end). The writes derive the memo's scope
// the way the handlers do (the request's claims, or none), so they share the request build's entries.
func TestStreamedTurn_WritesReuseTheHistoryScrub(t *testing.T) {
	cases := []struct {
		name  string
		scope string
		ctx   context.Context
		svc   func(t *testing.T, provider ai.Provider) (*ChatService, string)
	}{
		{"file store", "", context.Background(), fileChat},
		{"cloud store", "u1", asUser("u1"), func(t *testing.T, provider ai.Provider) (*ChatService, string) {
			svc, name := fileChat(t, provider)
			svc.backend, svc.mode = tu.NewFakeBackend(), config.ModeCloud
			return svc, name
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nonce := "n-" + uuid.NewString()
			calls := countingScrubMemo(t, nonce)
			provider := fakeAnswer(nonce + " the answer explains the token refresh: " + anchoredProse(3))
			svc, name := tc.svc(t, provider)
			doc := &models.FlowDocument{ID: "flow-scrub-memo", Name: "flow-scrub-memo"}
			convID := uuid.NewString()
			key := models.ChatRequest{ConversationID: convID}.ConversationKey()
			seed := anchoredHistory(8, 3, nonce)
			if err := svc.SaveConversation(tc.ctx, doc.ID, key, seed); err != nil {
				t.Fatalf("seed the thread: %v", err)
			}
			if n := calls.Load(); n != 8 {
				t.Fatalf("the seed write scrubbed %d texts, want 8", n)
			}

			req := models.ChatRequest{
				Provider: name, Model: "fake-model", ConversationID: convID,
				UserMessage:   nonce + " why does the token refresh fail? " + anchoredProse(3),
				UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString(), ClientStreamID: uuid.NewString(),
			}
			id, err := svc.StreamChatMessage(tc.ctx, tc.scope, doc, nil, req)
			if err != nil {
				t.Fatalf("StreamChatMessage: %v", err)
			}
			if ctl := waitStreamFinished(t, svc, id); !ctl.done || ctl.errMsg != "" {
				t.Fatalf("stream did not finish cleanly: done=%v err=%q", ctl.done, ctl.errMsg)
			}
			if n := calls.Load() - 8; n != 2 {
				t.Errorf("the turn scrubbed %d of the thread's texts, want 2 (the question once, the answer once): a write re-scrubbed the stored history", n)
			}
			stored, err := svc.GetConversation(tc.ctx, doc.ID, key)
			if err != nil {
				t.Fatalf("GetConversation: %v", err)
			}
			if len(stored) != 10 {
				t.Errorf("the thread holds %d messages after the turn, want 10", len(stored))
			}
		})
	}
}

// The write of a thread near the cap: 200 messages of 4.4 KB anchored prose, as a long support thread looks to the
// scrubber; the steady state of a turn's two writes once the memo holds the stored copies.
func BenchmarkNormalizeConversation_AnchoredHistory(b *testing.B) {
	history := anchoredHistory(200, 44, "bench")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		normalizeConversation("bench", history)
	}
}

// One pasted 200 KB log with URLs on every line and a Password= line, once stored masked.
func BenchmarkNormalizeConversation_LogPaste(b *testing.B) {
	var sb strings.Builder
	for i := 0; sb.Len() < 200<<10; i++ {
		sb.WriteString("2026-10-23T10:00:00Z INFO GET http://api.example.com/v1/items/" + strconv.Itoa(i) + " 200 12ms\n")
	}
	sb.WriteString("2026-10-23T10:00:01Z DEBUG connecting with Password=hunter2hunter2 to db1\n")
	paste := normalizeConversation("bench", []models.ChatMessage{{ID: "p", Role: "user", Content: sb.String()}})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		normalizeConversation("bench", paste)
	}
}

// The store's copy is the provider's copy, whatever the seam scrubs. The writes and the request build scrub
// through the same memo (historyScrubs), so a scrub the seam gains (the free-text pass over pasted PAD code: a
// SET Password TO line whose literal the regexes alone leave) reaches the stored copy without a second list of
// shapes to keep in step: the question goes to the store exactly as it went to the model.
func TestStreamedTurn_StoreHoldsTheProvidersCopy(t *testing.T) {
	const literal = "Winter#2024-u7Qx"
	question := "Why does the login step fail? The flow sets it like this:\nSET Password TO $'''" + literal + "'''"
	// A seam that masks the PAD literal, standing in for the free-text scrub.
	memo := newScrubMemo(1024, 8<<20)
	memo.scrub = func(s string) string { return strings.ReplaceAll(scrubber.ScrubText(s), literal, "[REDACTED]") }
	orig := historyScrubs
	historyScrubs = memo
	t.Cleanup(func() { historyScrubs = orig })

	provider := fakeAnswer("the answer")
	svc, name := fileChat(t, provider)
	doc := &models.FlowDocument{ID: "flow-scrub-seam", Name: "flow-scrub-seam"}
	req := models.ChatRequest{
		Provider: name, Model: "fake-model", ConversationID: uuid.NewString(), UserMessage: question,
		UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString(), ClientStreamID: uuid.NewString(),
	}
	ctx := asUser("u1")
	id, err := svc.StreamChatMessage(ctx, "u1", doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	if ctl := waitStreamFinished(t, svc, id); !ctl.done || ctl.errMsg != "" {
		t.Fatalf("stream did not finish cleanly: done=%v err=%q", ctl.done, ctl.errMsg)
	}

	stored, err := svc.GetConversation(ctx, doc.ID, req.ConversationKey())
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	got := findMessage(t, "the store", stored, req.UserMessageID).Content
	assertMasked(t, "the stored question", got, literal)
	reqs := provider.Requests()
	if len(reqs) == 0 {
		t.Fatal("the provider was not called")
	}
	// The question is the tail of the request's user turn (guidelines and context are prefixed to it).
	sent := reqs[0].Messages[len(reqs[0].Messages)-1].Content
	if !strings.HasSuffix(sent, got) {
		t.Errorf("the store and the provider hold different copies of the question:\n store: %q\n sent:  %q", got, sent)
	}
}
