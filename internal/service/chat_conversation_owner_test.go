package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/platform/apperr"
	storageif "pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-security-3: a conversation is its creator's. The tests run on the fake and, with DATABASE_URL, on Postgres
// (forEachCloudStore): users A and B chat about one flow.

const aliceSecret = "alice's merger plan for Q4"

// seedAliceThread stores A's thread under key through A's own client save, the way the client stores one.
func seedAliceThread(t *testing.T, svc *ChatService, flowID, key string) int64 {
	t.Helper()
	zero := int64(0)
	saved, err := svc.SaveClientConversation(asUser("alice"), flowID, key, &zero, []models.ChatMessage{
		{ID: uuid.NewString(), Role: "user", Content: aliceSecret},
		{ID: uuid.NewString(), Role: "assistant", Content: "noted: " + aliceSecret},
	}, false)
	if err != nil {
		t.Fatalf("A's save: %v", err)
	}
	return saved.Version
}

func mentionsAlice(msgs []models.ChatMessage) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, aliceSecret) {
			return true
		}
	}
	return false
}

// B reads A's thread by its key — a thread uuid, the legacy "flow" key, a block id — and gets an empty
// conversation at version 0, through GET and through the history a turn or preview rebuilds.
func TestConversationOwner_AnotherUsersThreadReadsEmpty(t *testing.T) {
	forEachCloudStore(t, func(t *testing.T, b chatStore) {
		svc := cloudChat(b)
		flowID := "flow-owner-read-" + uuid.NewString()[:8]
		for _, key := range []string{uuid.NewString(), "flow", uuid.NewString()} {
			seedAliceThread(t, svc, flowID, key)
			read, err := svc.ReadConversation(asUser("bob"), "bob", flowID, key)
			if err != nil {
				t.Fatalf("B's read: %v", err)
			}
			if len(read.Messages) != 0 || read.Version != 0 || mentionsAlice(read.Messages) {
				t.Errorf("key %s: B read (%d messages, version %d), want an empty conversation at 0", key, len(read.Messages), read.Version)
			}
			got, err := svc.GetConversation(asUser("bob"), flowID, key)
			if err != nil || len(got) != 0 {
				t.Errorf("key %s: B's history = (%d messages, %v), want none", key, len(got), err)
			}
			if own, err := svc.ReadConversation(asUser("alice"), "alice", flowID, key); err != nil || len(own.Messages) != 2 {
				t.Errorf("key %s: A's own read = (%d messages, %v), want 2", key, len(own.Messages), err)
			}
		}
	})
}

// B's stream with conversationId equal to A's thread id starts a conversation that holds none of A's history: the
// model gets no message of A's, and A's thread is left as it was (B's turn is not stored on A's key: the done says
// answerStored false, and B's client save of it is refused with CONVERSATION_KEY_TAKEN).
func TestConversationOwner_StreamOnAnotherUsersThreadStartsEmpty(t *testing.T) {
	forEachCloudStore(t, func(t *testing.T, b chatStore) {
		provider := fakeAnswer("hello bob")
		h := newStreamHarness(t, b, provider)
		v := seedAliceThread(t, h.svc, h.doc.ID, h.key)

		req := models.ChatRequest{
			Provider: h.provider, Model: "fake-model", ConversationID: h.convID, UserMessage: "what did we decide?",
			UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString(), ClientStreamID: uuid.NewString(),
		}
		id, err := h.svc.StreamChatMessage(asUser("bob"), "bob", h.doc, nil, req)
		if err != nil {
			t.Fatalf("B's stream: %v", err)
		}
		ctl := waitStreamFinished(t, h.svc, id)
		if !ctl.done || ctl.errMsg != "" {
			t.Fatalf("B's stream did not finish cleanly: done=%v err=%q", ctl.done, ctl.errMsg)
		}
		reqs := provider.Requests()
		if len(reqs) == 0 {
			t.Fatal("the provider was not called")
		}
		for _, m := range reqs[0].Messages {
			if strings.Contains(m.Content, aliceSecret) {
				t.Fatalf("B's turn sent A's history to the model: %q", m.Content)
			}
		}
		if !ctl.answerUnstored.Load() {
			t.Error("B's answer reads as stored, but A's key cannot hold it: done must say answerStored false")
		}
		stored, sv, err := b.LoadConversationVersioned(context.Background(), h.doc.ID, h.key, "")
		if err != nil {
			t.Fatal(err)
		}
		if sv != v || len(stored) != 2 {
			t.Errorf("A's thread changed under B's turn: %d messages at version %d, want 2 at %d", len(stored), sv, v)
		}
		zero := int64(0)
		_, err = h.svc.SaveClientConversation(asUser("bob"), h.doc.ID, h.key, &zero, []models.ChatMessage{{ID: req.UserMessageID, Role: "user", Content: req.UserMessage}}, false)
		if ae, ok := apperr.As(err); !ok || ae.Code != CodeConversationKeyTaken {
			t.Errorf("B's client save on A's key: err = %v, want %s", err, CodeConversationKeyTaken)
		}
	})
}

// The context preview rebuilds the history a turn would send: B's preview on A's key carries none of it.
func TestConversationOwner_PreviewOnAnotherUsersThreadHasNoHistory(t *testing.T) {
	forEachCloudStore(t, func(t *testing.T, b chatStore) {
		h := newStreamHarness(t, b, fakeAnswer("x"))
		seedAliceThread(t, h.svc, h.doc.ID, h.key)
		req := models.ChatRequest{Provider: h.provider, Model: "fake-model", ConversationID: h.convID, UserMessage: "hi"}
		res, err := h.svc.PreviewContext(asUser("bob"), "bob", h.doc, nil, req)
		if err != nil {
			t.Fatalf("B's preview: %v", err)
		}
		if res.HistoryMessages != 0 {
			t.Errorf("B's preview forwards %d history messages of A's thread, want 0", res.HistoryMessages)
		}
		mine, err := h.svc.PreviewContext(asUser("alice"), "alice", h.doc, nil, req)
		if err != nil || mine.HistoryMessages != 2 {
			t.Errorf("A's preview = (%d history messages, %v), want 2", mine.HistoryMessages, err)
		}
	})
}

// Every server-side write stamps the caller: A's streamed turn creates A's thread, which is in A's thread list and
// not in B's.
func TestConversationOwner_ServerWritesStampTheCaller(t *testing.T) {
	forEachCloudStore(t, func(t *testing.T, b chatStore) {
		h := newStreamHarness(t, b, fakeAnswer("the answer"))
		req := models.ChatRequest{
			Provider: h.provider, Model: "fake-model", ConversationID: h.convID, UserMessage: "question",
			UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString(), ClientStreamID: uuid.NewString(),
		}
		id, err := h.svc.StreamChatMessage(asUser("alice"), "alice", h.doc, nil, req)
		if err != nil {
			t.Fatalf("StreamChatMessage: %v", err)
		}
		if ctl := waitStreamFinished(t, h.svc, id); !ctl.done || ctl.answerUnstored.Load() {
			t.Fatalf("A's turn: done=%v answerUnstored=%v", ctl.done, ctl.answerUnstored.Load())
		}
		threads, err := h.svc.ListThreads(asUser("alice"), h.doc.ID, nil, 50)
		if err != nil {
			t.Fatalf("ListThreads: %v", err)
		}
		if len(threads) != 1 || threads[0].Scope != h.key || threads[0].MessageCount != 2 {
			t.Errorf("A's threads = %+v, want the streamed thread with 2 messages", threads)
		}
		if bobs, err := h.svc.ListThreads(asUser("bob"), h.doc.ID, nil, 50); err != nil || len(bobs) != 0 {
			t.Errorf("B's threads = (%+v, %v), want none", bobs, err)
		}
	})
}

// A client save (with or without baseVersion) on another user's key is refused with CONVERSATION_KEY_TAKEN — not
// the CONVERSATION_CONFLICT a client would answer by reloading and saving again, forever — and changes nothing.
func TestConversationOwner_ClientSaveOnAnotherUsersKeyIsKeyTaken(t *testing.T) {
	forEachCloudStore(t, func(t *testing.T, b chatStore) {
		svc := cloudChat(b)
		flowID, key := "flow-owner-save-"+uuid.NewString()[:8], uuid.NewString()
		v := seedAliceThread(t, svc, flowID, key)
		zero := int64(0)
		for name, base := range map[string]*int64{"base 0": &zero, "base A's version": &v, "blind": nil} {
			_, err := svc.SaveClientConversation(asUser("bob"), flowID, key, base, []models.ChatMessage{{ID: "b1", Role: "user", Content: "bob was here"}}, false)
			ae, ok := apperr.As(err)
			if !ok || ae.Code != CodeConversationKeyTaken || ae.Kind != apperr.KindConflict {
				t.Errorf("%s: err = %v, want a 409 %s", name, err, CodeConversationKeyTaken)
			}
		}
		if own, _ := svc.ReadConversation(asUser("alice"), "alice", flowID, key); own.Version != v || len(own.Messages) != 2 {
			t.Errorf("A's thread changed: %d messages at %d, want 2 at %d", len(own.Messages), own.Version, v)
		}
	})
}

// A clear is the caller's: B's clear of A's key (blind or at A's version) deletes nothing; A's deletes A's thread. An
// unowned (pre-owner) conversation is cleared only by a caller that may delete the flow (CONVERSATION_UNOWNED, 403,
// for anyone else).
func TestConversationOwner_ClearIsTheCallers(t *testing.T) {
	forEachCloudStore(t, func(t *testing.T, b chatStore) {
		svc := cloudChat(b)
		flowID, key := "flow-owner-clear-"+uuid.NewString()[:8], uuid.NewString()
		v := seedAliceThread(t, svc, flowID, key)

		if err := svc.ClearConversation(asUser("bob"), flowID, key, true); err != nil {
			t.Errorf("B's blind clear of A's key = %v, want the no-op of a missing conversation", err)
		}
		if err := svc.ClearConversationIf(asUser("bob"), flowID, key, v, true); err == nil {
			t.Error("B's clear at A's version succeeded")
		}
		if own, _ := svc.ReadConversation(asUser("alice"), "alice", flowID, key); own.Version != v {
			t.Fatalf("B's clear removed A's thread (version now %d)", own.Version)
		}
		// A viewer's own thread: no flow right needed.
		if err := svc.ClearConversationIf(asUser("alice"), flowID, key, v, false); err != nil {
			t.Fatalf("A's clear of A's thread: %v", err)
		}
		if got, _, _ := b.LoadConversationVersioned(context.Background(), flowID, key, ""); len(got) != 0 {
			t.Error("A's clear left the thread")
		}

		// An unowned conversation: a blind system write, as every row before migration 0052.
		if err := b.SaveConversation(context.Background(), flowID, "flow", []storageif.ChatMessage{{ID: "l1", Role: "user", Content: "legacy"}}); err != nil {
			t.Fatal(err)
		}
		for name, clear := range map[string]func() error{
			"blind": func() error { return svc.ClearConversation(asUser("bob"), flowID, "flow", false) },
			"versioned": func() error {
				_, lv, _ := b.LoadConversationVersioned(context.Background(), flowID, "flow", "")
				return svc.ClearConversationIf(asUser("bob"), flowID, "flow", lv, false)
			},
		} {
			err := clear()
			if ae, ok := apperr.As(err); !ok || ae.Code != CodeConversationUnowned || ae.Kind != apperr.KindForbidden {
				t.Errorf("%s clear of an unowned conversation without the flow right: err = %v, want a 403 %s", name, err, CodeConversationUnowned)
			}
		}
		if got, _, _ := b.LoadConversationVersioned(context.Background(), flowID, "flow", ""); len(got) != 1 {
			t.Fatal("a refused clear removed the unowned conversation")
		}
		if err := svc.ClearConversation(asUser("bob"), flowID, "flow", true); err != nil {
			t.Fatalf("clear with the flow right: %v", err)
		}
		if got, _, _ := b.LoadConversationVersioned(context.Background(), flowID, "flow", ""); len(got) != 0 {
			t.Error("the clear with the flow right left the unowned conversation")
		}
	})
}

// The thread index: a title is clamped (control and bidi characters dropped, whitespace collapsed, at most
// ConversationTitleMaxRunes) and stored as its scrubbed copy; another user's key is CONVERSATION_KEY_TAKEN.
func TestConversationThreads_TitleIsClampedAndScrubbed(t *testing.T) {
	forEachCloudStore(t, func(t *testing.T, b chatStore) {
		svc := cloudChat(b)
		flowID, key := "flow-owner-title-"+uuid.NewString()[:8], uuid.NewString()
		seedAliceThread(t, svc, flowID, key)
		long := strings.Repeat("ü", storageif.ConversationTitleMaxRunes+40)
		if _, err := svc.SetThreadTitle(asUser("alice"), flowID, key, "  Plan\u202e for\n\tQ4  "+long); err != nil {
			t.Fatalf("SetThreadTitle: %v", err)
		}
		threads, err := svc.ListThreads(asUser("alice"), flowID, nil, 10)
		if err != nil || len(threads) != 1 {
			t.Fatalf("ListThreads = (%+v, %v)", threads, err)
		}
		title := threads[0].Title
		if !strings.HasPrefix(title, "Plan for Q4 ü") || len([]rune(title)) != storageif.ConversationTitleMaxRunes {
			t.Errorf("title = %q (%d runes), want the clamped %d-rune title", title, len([]rune(title)), storageif.ConversationTitleMaxRunes)
		}
		if _, err := svc.SetThreadTitle(asUser("alice"), flowID, key, "key "+bearerText); err != nil {
			t.Fatalf("SetThreadTitle: %v", err)
		}
		threads, _ = svc.ListThreads(asUser("alice"), flowID, nil, 10)
		if len(threads) != 1 || strings.Contains(threads[0].Title, bearerValue) {
			t.Errorf("a title with a token is stored as %q, want it masked", threads[0].Title)
		}
		_, err = svc.SetThreadTitle(asUser("bob"), flowID, key, "mine now")
		if ae, ok := apperr.As(err); !ok || ae.Code != CodeConversationKeyTaken {
			t.Errorf("B naming A's thread: err = %v, want %s", err, CodeConversationKeyTaken)
		}
	})
}

// The file store (desktop, local modes) has no thread index: the list is empty and a title is accepted and dropped.
func TestConversationThreads_FileStoreHasNoIndex(t *testing.T) {
	svc := &ChatService{configDir: t.TempDir()}
	if got, err := svc.ListThreads(context.Background(), "flow-1", nil, 10); err != nil || len(got) != 0 {
		t.Errorf("file store list = (%v, %v), want empty", got, err)
	}
	if _, err := svc.SetThreadTitle(context.Background(), "flow-1", uuid.NewString(), "x"); err != nil {
		t.Errorf("file store title = %v, want nil", err)
	}
}

// A conversation stored before owners under a predictable key ("flow", a block id) stays shared: a client save,
// blind or at its version, may not replace its list unless the caller may delete the flow (403
// CONVERSATION_UNOWNED); the server's own appends still reach it. Under a client-generated thread id the save claims
// it instead (R-security-3).
func TestConversationOwner_SharedUnownedRowIsNotReplacedByAClient(t *testing.T) {
	forEachCloudStore(t, func(t *testing.T, b chatStore) {
		svc := cloudChat(b)
		flowID := "flow-owner-shared-" + uuid.NewString()[:8]
		ctx := context.Background()
		if err := b.SaveConversation(ctx, flowID, "flow", []storageif.ChatMessage{{ID: "l1", Role: "user", Content: aliceSecret}}); err != nil {
			t.Fatal(err)
		}
		_, v, _ := b.LoadConversationVersioned(ctx, flowID, "flow", "")
		for name, base := range map[string]*int64{"blind": nil, "at its version": &v} {
			_, err := svc.SaveClientConversation(asUser("bob"), flowID, "flow", base, []models.ChatMessage{{ID: "b1", Role: "user", Content: "wiped"}}, false)
			if ae, ok := apperr.As(err); !ok || ae.Code != CodeConversationUnowned {
				t.Errorf("%s: B's save over the shared conversation: err = %v, want %s", name, err, CodeConversationUnowned)
			}
		}
		if got, gv, _ := b.LoadConversationVersioned(ctx, flowID, "flow", ""); gv != v || len(got) != 1 {
			t.Fatalf("a refused save changed the shared conversation: %d messages at %d", len(got), gv)
		}
		// The server's append (a turn on the legacy key) still works and keeps it shared.
		if _, err := svc.mutateConversation(asUser("bob"), flowID, "flow", func(h []models.ChatMessage) ([]models.ChatMessage, bool, error) {
			return append(h, models.ChatMessage{ID: "b2", Role: "user", Content: "bob's turn"}), true, nil
		}); err != nil {
			t.Fatalf("the server's append: %v", err)
		}
		if got, _ := svc.GetConversation(asUser("alice"), flowID, "flow"); len(got) != 2 {
			t.Errorf("A reads %d messages, want the shared conversation with B's turn", len(got))
		}
		// The flow's owner (or an org admin) may replace it.
		if _, err := svc.SaveClientConversation(asUser("owner"), flowID, "flow", nil, []models.ChatMessage{{ID: "o1", Role: "user", Content: "kept"}}, true); err != nil {
			t.Errorf("a save with the flow right: %v", err)
		}
		// Under a thread id the save claims the unowned thread for its holder.
		key := uuid.NewString()
		if err := b.SaveConversation(ctx, flowID, key, []storageif.ChatMessage{{ID: "l1", Role: "user", Content: aliceSecret}}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SaveClientConversation(asUser("alice"), flowID, key, nil, []models.ChatMessage{{ID: "l1", Role: "user", Content: aliceSecret}, {ID: "a2", Role: "user", Content: "more"}}, false); err != nil {
			t.Fatalf("A's save of the unowned thread under a thread id: %v", err)
		}
		if got, _ := svc.GetConversation(asUser("bob"), flowID, key); len(got) != 0 {
			t.Error("the claimed thread is still readable by B")
		}
	})
}

// A title respects "Save conversation history" (off: nothing stored), creates a conversation only under a thread id
// (never under "flow" or another predictable key, which would take the key from every other user), and does not bring
// back a thread the caller cleared.
func TestConversationThreads_TitleCreatesOnlyLiveThreads(t *testing.T) {
	backend := &userPrefsBackend{FakeBackend: tu.NewFakeBackend(), prefs: map[string]*storageif.AppSettings{
		"quiet": {AI: storageif.AISettings{SaveConversationHistory: false}},
	}}
	svc := cloudChat(backend)
	ctx := context.Background()
	flowID := "flow-title-live"
	exists := func(key string) bool {
		_, v, _ := backend.LoadConversationVersioned(ctx, flowID, key, "")
		return v != 0
	}

	quietKey := uuid.NewString()
	saved, err := svc.SetThreadTitle(asUser("quiet"), flowID, quietKey, "private")
	if err != nil || saved.Versioned || exists(quietKey) {
		t.Errorf("history off: (%+v, %v), stored %v; want accepted and dropped", saved, err, exists(quietKey))
	}

	for _, key := range []string{"flow", "not-a-uuid", "0B7C4C1E-6A65-4CF4-9A49-6F7D3B1F2A10"} {
		saved, err := svc.SetThreadTitle(asUser("alice"), flowID, key, "squat")
		if err != nil || saved.Versioned || exists(key) {
			t.Errorf("%q: (%+v, %v), stored %v; want nothing created under a key that is not a thread id", key, saved, err, exists(key))
		}
	}

	live := uuid.NewString()
	saved, err = svc.SetThreadTitle(asUser("alice"), flowID, live, "named first")
	if err != nil || !saved.Versioned || !exists(live) {
		t.Fatalf("a live thread named before its first turn: (%+v, %v), want it created with its version", saved, err)
	}
	if err := svc.ClearConversationIf(asUser("alice"), flowID, live, saved.Version, false); err != nil {
		t.Fatalf("clear: %v", err)
	}
	saved, err = svc.SetThreadTitle(asUser("alice"), flowID, live, "renamed after closing")
	if err != nil || saved.Versioned || exists(live) {
		t.Errorf("a title after the clear: (%+v, %v), stored %v; want the cleared thread left gone", saved, err, exists(live))
	}
}

// deletionCtxBackend records whether DeleteFlow was told the deletion is authorised.
type deletionCtxBackend struct {
	*tu.FakeBackend
	authorised []bool
}

func (b *deletionCtxBackend) DeleteFlow(ctx context.Context, id string) error {
	b.authorised = append(b.authorised, storageif.FlowDeletionAuthorized(ctx, id))
	return b.FakeBackend.DeleteFlow(ctx, id)
}

// The library's delete states, after its FlowDelete check, that the deletion is authorised: the store deletes every
// user's conversations on the flow as the system only on that word (R-security-3).
func TestLibraryDelete_StatesTheAuthorisation(t *testing.T) {
	backend := &deletionCtxBackend{FakeBackend: tu.NewFakeBackend()}
	seedCloudFlow(t, backend.FakeBackend, "f1")
	lib := NewLibraryService(backend, nil, NewAuthzService(backend, nil), nil)
	if _, err := lib.DeleteLibraryFlow(actorCtx("u1"), "f1"); err != nil {
		t.Fatalf("DeleteLibraryFlow: %v", err)
	}
	if len(backend.authorised) != 1 || !backend.authorised[0] {
		t.Errorf("DeleteFlow saw the authorisation %v, want it stated once", backend.authorised)
	}
}
