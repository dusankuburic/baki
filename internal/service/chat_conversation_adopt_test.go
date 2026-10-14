package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
	"pad-core/models"
)

// The stored conversation version leaves the server with the stream's terminal event (ADR 0031, addendum): the
// answer is stored BEFORE `done` is emitted and the event names the version it was stored at, so the client has
// nothing left to save and nothing stale to reconcile.

// doneSpyNotifier records every chat event and, at the moment a `done` or `error` event is handed to the transport,
// reads what the store holds, so a test can tell whether the write came before the event.
type doneSpyNotifier struct {
	*tu.CountingNotifier
	backend storageif.ConversationStore
	flowID  string
	key     string

	mu       sync.Mutex
	atTerm   map[string][]storageif.ChatMessage // event type -> stored messages when it was emitted
	terminal []map[string]any                   // data of each done / error, in order
}

func (n *doneSpyNotifier) EmitTo(userID, name string, data any) {
	if payload, ok := data.(map[string]interface{}); ok && name == "chat:event" {
		if typ, _ := payload["type"].(string); typ == "done" || typ == "error" {
			stored, _ := n.backend.LoadConversation(context.Background(), n.flowID, n.key)
			n.mu.Lock()
			if n.atTerm == nil {
				n.atTerm = map[string][]storageif.ChatMessage{}
			}
			n.atTerm[typ] = stored
			if d, ok := payload["data"].(map[string]interface{}); ok {
				n.terminal = append(n.terminal, d)
			}
			n.mu.Unlock()
		}
	}
	n.CountingNotifier.EmitTo(userID, name, data)
}

func (n *doneSpyNotifier) terminalEvents() []map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]map[string]any(nil), n.terminal...)
}

func (n *doneSpyNotifier) storedAt(typ string) []storageif.ChatMessage {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.atTerm[typ]
}

type streamHarness struct {
	provider string
	svc      *ChatService
	spy      *doneSpyNotifier
	doc      *models.FlowDocument
	convID   string
	key      string
	backend  storageif.ConversationStore
}

func newStreamHarness(t *testing.T, backend chatStore, provider ai.Provider) *streamHarness {
	t.Helper()
	name := "cv-adopt-" + uuid.NewString()
	ai.RegisterProvider(name, func(string) ai.Provider { return provider })
	doc := &models.FlowDocument{ID: "flow-adopt"}
	convID := uuid.NewString()
	key := models.ChatRequest{ConversationID: convID}.ConversationKey()
	spy := &doneSpyNotifier{CountingNotifier: &tu.CountingNotifier{}, backend: backend, flowID: doc.ID, key: key}
	svc := &ChatService{
		notifier: spy, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
		backend: backend, mode: config.ModeCloud,
	}
	return &streamHarness{provider: name, svc: svc, spy: spy, doc: doc, convID: convID, key: key, backend: backend}
}

// turn runs one server-owned turn to the end of its stream.
func (h *streamHarness) turn(t *testing.T, text string) (models.ChatRequest, *streamCtl) {
	t.Helper()
	req := models.ChatRequest{
		Provider: h.provider, Model: "fake-model", ConversationID: h.convID, UserMessage: text,
		UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString(), ClientStreamID: uuid.NewString(),
	}
	id, err := h.svc.StreamChatMessage(context.Background(), "u1", h.doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	return req, waitStreamFinished(t, h.svc, id)
}

func number(t *testing.T, ev map[string]any, field string) (int64, bool) {
	t.Helper()
	v, ok := ev[field]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	t.Fatalf("%s is %T, want a number", field, v)
	return 0, false
}

func fakeAnswer(text string) *tu.FakeProvider {
	return &tu.FakeProvider{Fallback: &tu.FakeTurn{Text: text, TokensIn: 3, TokensOut: 2}}
}

// The answer is in the store when `done` is emitted, and `done` names the version it was stored at and the version
// the turn started from.
func TestStreamedTurn_AnswerIsStoredBeforeDoneAndDoneCarriesTheVersion(t *testing.T) {
	backend := tu.NewFakeBackend()
	h := newStreamHarness(t, backend, fakeAnswer("the answer"))

	req, ctl := h.turn(t, "question")
	if !ctl.done || ctl.errMsg != "" {
		t.Fatalf("stream did not finish cleanly: done=%v err=%q", ctl.done, ctl.errMsg)
	}

	atDone := h.spy.storedAt("done")
	if len(atDone) != 2 || atDone[1].ID != req.AssistantMessageID {
		t.Fatalf("the store held %d messages when done was emitted, want the question and the answer already there: %+v", len(atDone), atDone)
	}
	events := h.spy.terminalEvents()
	if len(events) != 1 {
		t.Fatalf("saw %d terminal events, want one done", len(events))
	}
	stored, version, err := backend.LoadConversationVersioned(context.Background(), h.doc.ID, h.key)
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored conversation = %d messages, %v", len(stored), err)
	}
	got, ok := number(t, events[0], "conversationVersion")
	if !ok || got != version {
		t.Errorf("done conversationVersion = %d (present %v), want the stored version %d", got, ok, version)
	}
	if base, ok := number(t, events[0], "conversationBase"); !ok || base != 0 {
		t.Errorf("done conversationBase = %d (present %v), want 0: the thread did not exist when the turn began", base, ok)
	}
}

// Two turns in a row: the second turn starts from the version the first one's done named, so a client that adopts
// each done never holds a version the server has moved past, and the server writes the thread exactly twice per turn.
func TestStreamedTurn_TwoTurnsInARowChainTheVersions(t *testing.T) {
	backend := &savesCountingBackend{FakeBackend: tu.NewFakeBackend()}
	h := newStreamHarness(t, backend, fakeAnswer("answer"))

	h.turn(t, "first")
	h.turn(t, "second")

	events := h.spy.terminalEvents()
	if len(events) != 2 {
		t.Fatalf("saw %d terminal events, want two dones", len(events))
	}
	v1, ok1 := number(t, events[0], "conversationVersion")
	base2, ok2 := number(t, events[1], "conversationBase")
	v2, ok3 := number(t, events[1], "conversationVersion")
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("version fields missing: %+v", events)
	}
	if base2 != v1 {
		t.Errorf("second turn began from %d, want the first done's version %d", base2, v1)
	}
	if v2 <= v1 {
		t.Errorf("second done version %d did not move past %d", v2, v1)
	}
	if n := backend.saves.Load(); n != 4 {
		t.Errorf("two turns wrote the conversation %d times, want 4 (question and answer each)", n)
	}
	_, current, _ := backend.LoadConversationVersioned(context.Background(), h.doc.ID, h.key)
	if current != v2 {
		t.Errorf("the thread is at %d, the last done said %d", current, v2)
	}
}

// peerBeforeSaveBackend appends a message from "another replica" right before the nth compare-and-swap save, so that
// save loses and the writer must run its mutation again on what the peer left.
type peerBeforeSaveBackend struct {
	*tu.FakeBackend
	nth   int32
	saves atomic.Int32
}

func (b *peerBeforeSaveBackend) SaveConversationIf(ctx context.Context, flowID, scope string, expected int64, msgs []storageif.ChatMessage) (int64, error) {
	if b.saves.Add(1) == b.nth {
		cur, _, err := b.LoadConversationVersioned(ctx, flowID, scope)
		if err != nil {
			return 0, err
		}
		if err := b.SaveConversation(ctx, flowID, scope, append(cur, storageif.ChatMessage{ID: "peer", Role: "user", Content: "from another tab"})); err != nil {
			return 0, err
		}
	}
	return b.FakeBackend.SaveConversationIf(ctx, flowID, scope, expected, msgs)
}

// Another writer got in between the question write and the answer write: the stored list holds a message this
// client has not seen, so `done` must not claim the client is in sync with the version.
func TestStreamedTurn_APeerWriteBetweenQuestionAndAnswerWithholdsTheVersion(t *testing.T) {
	noBackoff(t)
	backend := &peerBeforeSaveBackend{FakeBackend: tu.NewFakeBackend(), nth: 2}
	h := newStreamHarness(t, backend, fakeAnswer("the answer"))

	req, _ := h.turn(t, "question")

	stored, _, _ := backend.LoadConversationVersioned(context.Background(), h.doc.ID, h.key)
	if len(stored) != 3 {
		t.Fatalf("stored %d messages, want the question, the peer's message and the answer", len(stored))
	}
	events := h.spy.terminalEvents()
	if len(events) != 1 {
		t.Fatalf("saw %d terminal events", len(events))
	}
	if _, ok := events[0]["conversationVersion"]; ok {
		t.Errorf("done carries a version although a peer wrote during the turn: %+v", events[0])
	}
	if stored[1].ID != req.AssistantMessageID || stored[2].ID != "peer" {
		t.Errorf("the answer is not stored right after its question, ahead of the peer's message: %+v", stored)
	}
}

// A failed turn still stored its question at stream start: the error names that version so the client's next
// compaction or clear is not made on a stale one.
func TestStreamedTurn_ErrorCarriesTheQuestionsVersion(t *testing.T) {
	backend := tu.NewFakeBackend()
	h := newStreamHarness(t, backend, &tu.FakeProvider{Err: errors.New("provider exploded")})

	_, ctl := h.turn(t, "question")
	if ctl.errMsg == "" {
		t.Fatal("the stream did not fail")
	}
	events := h.spy.terminalEvents()
	if len(events) != 1 {
		t.Fatalf("saw %d terminal events, want one error", len(events))
	}
	stored, version, _ := backend.LoadConversationVersioned(context.Background(), h.doc.ID, h.key)
	if len(stored) != 1 {
		t.Fatalf("stored %d messages, want only the question", len(stored))
	}
	if got, ok := number(t, events[0], "conversationVersion"); !ok || got != version {
		t.Errorf("error conversationVersion = %d (present %v), want %d", got, ok, version)
	}
	if base, ok := number(t, events[0], "conversationBase"); !ok || base != 0 {
		t.Errorf("error conversationBase = %d (present %v), want 0", base, ok)
	}
}

// A client-owned turn (the client sent the history) stores nothing on the server, so its terminal events name no
// version: the client keeps owning the save.
func TestStreamedTurn_ClientOwnedTurnCarriesNoVersion(t *testing.T) {
	backend := tu.NewFakeBackend()
	h := newStreamHarness(t, backend, fakeAnswer("the answer"))
	req := models.ChatRequest{
		Provider: h.provider, Model: "fake-model", ConversationID: h.convID, UserMessage: "question",
		Messages:       []models.ChatMessage{{ID: "p", Role: "user", Content: "earlier"}},
		ClientStreamID: uuid.NewString(),
	}
	id, err := h.svc.StreamChatMessage(context.Background(), "u1", h.doc, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	waitStreamFinished(t, h.svc, id)

	events := h.spy.terminalEvents()
	if len(events) != 1 {
		t.Fatalf("saw %d terminal events", len(events))
	}
	if _, ok := events[0]["conversationVersion"]; ok {
		t.Errorf("a client-owned turn announced a version: %+v", events[0])
	}
}

// L2: the user's question is stored once. When its id is already in the stored thread (a late client save of the
// same turn, or a retried round), the turn does not store it again, and the model's context stops before it.
func TestPersistUserTurn_AnIdAlreadyStoredIsNotStoredAgain(t *testing.T) {
	backend := tu.NewFakeBackend()
	svc := cloudChat(backend)
	ctx := context.Background()
	doc := &models.FlowDocument{ID: "f"}
	userID := uuid.NewString()
	seed := []storageif.ChatMessage{
		{ID: "p1", Role: "user", Content: "earlier"},
		{ID: "p2", Role: "assistant", Content: "earlier answer"},
		{ID: userID, Role: "user", Content: "the question"},
	}
	if err := backend.SaveConversation(ctx, "f", "k", seed); err != nil {
		t.Fatal(err)
	}
	_, before, _ := backend.LoadConversationVersioned(ctx, "f", "k")

	req := models.ChatRequest{ConversationID: uuid.NewString(), UserMessage: "the question", UserMessageID: userID}
	stored, _ := svc.reconstructAndPersistUserTurn(ctx, doc, &req, "k")
	if !stored {
		t.Error("the question is in the store but the turn reports it as not stored")
	}

	got, after, _ := backend.LoadConversationVersioned(ctx, "f", "k")
	if len(got) != 3 {
		t.Errorf("the thread has %d messages, want the same 3 (no duplicate question): %+v", len(got), got)
	}
	if after != before {
		t.Errorf("version moved %d -> %d although nothing changed", before, after)
	}
	if len(req.Messages) != 2 || req.Messages[1].ID != "p2" {
		t.Errorf("model context = %+v, want the two messages before the question", req.Messages)
	}
}

// L3: running out of rounds is counted and logged, not silent.
func TestMutateConversation_GivingUpIsCounted(t *testing.T) {
	noBackoff(t)
	backend := &interleavingBackend{FakeBackend: tu.NewFakeBackend(), times: 1000, peer: storageif.ChatMessage{ID: "peer", Role: "user"}}
	svc := cloudChat(backend)
	before := testutil.ToFloat64(chatPersistGiveUpsTotal)

	_, err := svc.mutateConversation(context.Background(), "f", "k", func(h []models.ChatMessage) ([]models.ChatMessage, bool, error) {
		return append(h, models.ChatMessage{ID: "mine", Role: "user", Content: "x"}), true, nil
	})
	if !errors.Is(err, storageif.ErrConversationConflict) {
		t.Fatalf("err = %v, want ErrConversationConflict", err)
	}
	if got := testutil.ToFloat64(chatPersistGiveUpsTotal) - before; got != 1 {
		t.Errorf("pad_chat_persist_giveups_total rose by %v, want 1", got)
	}
}
