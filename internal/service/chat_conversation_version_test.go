package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/apperr"
	storageif "pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
	"pad-core/models"
)

// staleReadBackend reads a conversation and then holds the answer for a moment before returning it, so every
// reader is stale by the time it writes. Two ChatService values over one such backend are two replicas that read
// the same list and then both write it back.
type staleReadBackend struct {
	*tu.FakeBackend
	hold time.Duration
}

func (b *staleReadBackend) LoadConversation(ctx context.Context, flowID, scope string) ([]storageif.ChatMessage, error) {
	msgs, err := b.FakeBackend.LoadConversation(ctx, flowID, scope)
	time.Sleep(b.hold)
	return msgs, err
}

func (b *staleReadBackend) LoadConversationVersioned(ctx context.Context, flowID, scope, owner string) ([]storageif.ChatMessage, int64, error) {
	msgs, v, err := b.FakeBackend.LoadConversationVersioned(ctx, flowID, scope, owner)
	time.Sleep(b.hold)
	return msgs, v, err
}

func cloudChat(backend chatStore) *ChatService {
	return &ChatService{backend: backend, mode: config.ModeCloud, configDir: ""}
}

// finishedAnswer is a stream control whose answer is complete, the input persistAssistantTurn stores.
func finishedAnswer(text string) *streamCtl {
	ctl := &streamCtl{done: true}
	ctl.buffer.WriteString(text)
	return ctl
}

// Two replicas (two services: separate in-process locks) write the same conversation from many goroutines: the
// user turn at stream start and the assistant answer at stream end. Every turn must be stored exactly once. With a
// read-modify-write that replaces the whole list, the later writer erases the earlier one's message.
func TestTwoReplicas_UserAndAssistantTurnsAreNeverLost(t *testing.T) {
	backend := &staleReadBackend{FakeBackend: tu.NewFakeBackend(), hold: 2 * time.Millisecond}
	replicas := []*ChatService{cloudChat(backend), cloudChat(backend)}
	doc := &models.FlowDocument{ID: "flow-cv"}
	convID := uuid.NewString()
	key := models.ChatRequest{ConversationID: convID}.ConversationKey()

	const turns = 8
	var want []string
	var wg sync.WaitGroup
	for i := range turns {
		userID, answerID := uuid.NewString(), uuid.NewString()
		want = append(want, userID, answerID)
		svc := replicas[i%2]
		wg.Add(2)
		go func() {
			defer wg.Done()
			req := models.ChatRequest{ConversationID: convID, UserMessage: fmt.Sprintf("question %d", i), UserMessageID: userID}
			svc.reconstructAndPersistUserTurn(context.Background(), doc, &req, key)
		}()
		go func() {
			defer wg.Done()
			req := models.ChatRequest{ConversationID: convID, AssistantMessageID: answerID}
			replicas[(i+1)%2].persistAssistantTurn(context.Background(), doc, req, finishedAnswer(fmt.Sprintf("answer %d", i)), "", "")
		}()
	}
	wg.Wait()

	got, err := replicas[0].GetConversation(context.Background(), doc.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, m := range got {
		counts[m.ID]++
	}
	for _, id := range want {
		if counts[id] != 1 {
			t.Errorf("message %s stored %d times, want once (history has %d messages, want %d)", id, counts[id], len(got), len(want))
		}
	}
}

// interleavingBackend makes the first n compare-and-swap saves lose to a write that lands in between, as a second
// replica's would: it appends peer to the stored list before the save runs.
type interleavingBackend struct {
	*tu.FakeBackend
	peer  storageif.ChatMessage
	times int
	saves atomic.Int32
}

func (b *interleavingBackend) SaveConversationIf(ctx context.Context, flowID, scope, owner string, expected int64, msgs []storageif.ChatMessage) (int64, error) {
	if int(b.saves.Add(1)) <= b.times {
		cur, _, err := b.LoadConversationVersioned(ctx, flowID, scope, owner)
		if err != nil {
			return 0, err
		}
		if err := b.SaveConversation(ctx, flowID, scope, append(cur, b.peer)); err != nil {
			return 0, err
		}
	}
	return b.FakeBackend.SaveConversationIf(ctx, flowID, scope, owner, expected, msgs)
}

func noBackoff(t *testing.T) {
	t.Helper()
	prev := mutateConversationBackoff
	mutateConversationBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { mutateConversationBackoff = prev })
}

// A save that loses to another writer runs the mutation again on the list that writer left, so its change lands on
// top of it instead of replacing it.
func TestMutateConversation_ConflictReappliesToTheFreshHistory(t *testing.T) {
	noBackoff(t)
	backend := &interleavingBackend{FakeBackend: tu.NewFakeBackend(), times: 2, peer: storageif.ChatMessage{ID: "peer", Role: "user", Content: "from another replica"}}
	svc := cloudChat(backend)
	ctx := context.Background()
	if err := backend.SaveConversation(ctx, "f", "k", []storageif.ChatMessage{{ID: "m0", Role: "user", Content: "first"}}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	stored, err := svc.mutateConversation(ctx, "f", "k", func(h []models.ChatMessage) ([]models.ChatMessage, bool, error) {
		calls++
		return append(h, models.ChatMessage{ID: "mine", Role: "user", Content: "mine"}), true, nil
	})
	if err != nil || !stored {
		t.Fatalf("mutateConversation = (%v, %v), want it stored", stored, err)
	}
	if calls != 3 {
		t.Errorf("mutation ran %d times, want 3 (two lost races, one win)", calls)
	}
	got, _ := svc.GetConversation(ctx, "f", "k")
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	// The peer wrote twice (one per lost race): both its messages and ours survive.
	if want := []string{"m0", "peer", "peer", "mine"}; !slices.Equal(ids, want) {
		t.Errorf("stored %v, want %v", ids, want)
	}
}

// The retry is bounded: a conversation that keeps changing under the writer ends in an error the caller logs, not a
// loop.
func TestMutateConversation_GivesUpAfterTheAttemptBound(t *testing.T) {
	noBackoff(t)
	backend := &interleavingBackend{FakeBackend: tu.NewFakeBackend(), times: 1000, peer: storageif.ChatMessage{ID: "peer", Role: "user"}}
	svc := cloudChat(backend)
	calls := 0
	stored, err := svc.mutateConversation(context.Background(), "f", "k", func(h []models.ChatMessage) ([]models.ChatMessage, bool, error) {
		calls++
		return append(h, models.ChatMessage{ID: "mine", Role: "user", Content: "x"}), true, nil
	})
	if stored || !errors.Is(err, storageif.ErrConversationConflict) {
		t.Fatalf("mutateConversation = (%v, %v), want not stored with ErrConversationConflict", stored, err)
	}
	if calls != mutateConversationAttempts {
		t.Errorf("mutation ran %d times, want %d attempts", calls, mutateConversationAttempts)
	}
}

// A mutation that changes nothing (the answer is already stored) writes nothing and does not move the version.
func TestMutateConversation_NoChangeWritesNothing(t *testing.T) {
	backend := tu.NewFakeBackend()
	svc := cloudChat(backend)
	ctx := context.Background()
	if err := backend.SaveConversation(ctx, "f", "k", []storageif.ChatMessage{{ID: "m0", Role: "user", Content: "first"}}); err != nil {
		t.Fatal(err)
	}
	_, before, _ := backend.LoadConversationVersioned(ctx, "f", "k", "")
	stored, err := svc.mutateConversation(ctx, "f", "k", func(h []models.ChatMessage) ([]models.ChatMessage, bool, error) { return h, false, nil })
	if stored || err != nil {
		t.Fatalf("mutateConversation = (%v, %v), want (false, nil)", stored, err)
	}
	if _, after, _ := backend.LoadConversationVersioned(ctx, "f", "k", ""); after != before {
		t.Errorf("version moved %d -> %d", before, after)
	}
}

// W3: a client save carrying a base version older than the stored one is refused with the conflict code and the
// current version, and writes nothing.
func TestSaveClientConversation_StaleBaseVersionConflicts(t *testing.T) {
	svc := cloudChat(tu.NewFakeBackend())
	ctx := context.Background()
	first := []models.ChatMessage{{ID: "m1", Role: "user", Content: "one"}}

	saved, err := svc.SaveClientConversation(ctx, "f", "k", new(int64), first, true)
	if err != nil || !saved.Versioned || saved.Version <= 0 {
		t.Fatalf("create at base 0 = (%+v, %v), want a versioned save", saved, err)
	}
	second := append(slices.Clone(first), models.ChatMessage{ID: "m2", Role: "assistant", Content: "two"})
	base := saved.Version
	next, err := svc.SaveClientConversation(ctx, "f", "k", &base, second, true)
	if err != nil || next.Version <= saved.Version {
		t.Fatalf("save at the current version = (%+v, %v), want a newer version", next, err)
	}

	stale := saved.Version
	_, err = svc.SaveClientConversation(ctx, "f", "k", &stale, []models.ChatMessage{{ID: "m9", Role: "user", Content: "overwrites"}}, true)
	ae, ok := apperr.As(err)
	if !ok || ae.Kind != apperr.KindConflict || ae.Code != CodeConversationConflict {
		t.Fatalf("stale save err = %v, want a 409 %s", err, CodeConversationConflict)
	}
	if ae.Details["version"] != next.Version {
		t.Errorf("details.version = %v, want the current %d", ae.Details["version"], next.Version)
	}
	if !errors.Is(err, storageif.ErrConversationConflict) {
		t.Errorf("the cause is lost: %v", err)
	}
	got, _ := svc.GetConversation(ctx, "f", "k")
	if len(got) != 2 {
		t.Errorf("a refused save changed the history: %d messages", len(got))
	}
}

// Old clients send no base version: the save stays the blind full replace it was, and is counted so the contract
// step can be scheduled once the counter reaches zero.
func TestSaveClientConversation_WithoutABaseVersionIsBlindAndCounted(t *testing.T) {
	svc := cloudChat(tu.NewFakeBackend())
	ctx := context.Background()
	before := testutil.ToFloat64(chatBlindSavesTotal)

	saved, err := svc.SaveClientConversation(ctx, "f", "k", nil, []models.ChatMessage{{ID: "m1", Role: "user", Content: "one"}}, true)
	if err != nil || saved.Versioned {
		t.Fatalf("blind save = (%+v, %v), want it accepted without a version", saved, err)
	}
	if _, err := svc.SaveClientConversation(ctx, "f", "k", nil, []models.ChatMessage{{ID: "m2", Role: "user", Content: "replaced"}}, true); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(chatBlindSavesTotal) - before; got != 2 {
		t.Errorf("pad_chat_blind_saves_total rose by %v, want 2", got)
	}
	got, _ := svc.GetConversation(ctx, "f", "k")
	if len(got) != 1 || got[0].ID != "m2" {
		t.Errorf("blind save did not replace the list: %+v", got)
	}

	// A versioned save is not counted.
	_, v, _ := svc.backend.LoadConversationVersioned(ctx, "f", "k", "")
	if _, err := svc.SaveClientConversation(ctx, "f", "k", &v, []models.ChatMessage{{ID: "m3", Role: "user", Content: "x"}}, true); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(chatBlindSavesTotal) - before; got != 2 {
		t.Errorf("a versioned save moved pad_chat_blind_saves_total to +%v", got)
	}
}

// W4: clearing with a stale version is refused; at the current version it deletes; without one it is the blind
// delete it was.
func TestClearConversationIf(t *testing.T) {
	svc := cloudChat(tu.NewFakeBackend())
	ctx := context.Background()
	base := int64(0)
	saved, err := svc.SaveClientConversation(ctx, "f", "k", &base, []models.ChatMessage{{ID: "m1", Role: "user", Content: "one"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	stale := saved.Version - 1
	err = svc.ClearConversationIf(ctx, "f", "k", stale, true)
	if ae, ok := apperr.As(err); !ok || ae.Code != CodeConversationConflict || ae.Details["version"] != saved.Version {
		t.Fatalf("stale clear err = %v, want %s carrying version %d", err, CodeConversationConflict, saved.Version)
	}
	if got, _ := svc.GetConversation(ctx, "f", "k"); len(got) != 1 {
		t.Fatalf("a refused clear deleted the history")
	}
	if err := svc.ClearConversationIf(ctx, "f", "k", saved.Version, true); err != nil {
		t.Fatalf("clear at the current version: %v", err)
	}
	if got, _ := svc.GetConversation(ctx, "f", "k"); len(got) != 0 {
		t.Errorf("history survived a clear at the current version: %d messages", len(got))
	}
}

// GET reports the version the messages were read at, and only where the store has versions.
func TestReadConversation_CarriesTheVersion(t *testing.T) {
	ctx := context.Background()
	cloud := cloudChat(tu.NewFakeBackend())
	empty, err := cloud.ReadConversation(ctx, "u1", "f", "k")
	if err != nil || !empty.Versioned || empty.Version != 0 || len(empty.Messages) != 0 {
		t.Fatalf("missing conversation = (%+v, %v), want an empty list at version 0", empty, err)
	}
	saved, err := cloud.SaveClientConversation(ctx, "f", "k", new(int64), []models.ChatMessage{{ID: "m1", Role: "user", Content: "one"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	read, err := cloud.ReadConversation(ctx, "u1", "f", "k")
	if err != nil || read.Version != saved.Version || len(read.Messages) != 1 {
		t.Errorf("read = (%+v, %v), want version %d with the message", read, err, saved.Version)
	}

	local := &ChatService{configDir: t.TempDir()}
	got, err := local.ReadConversation(ctx, "u1", "f", "k")
	if err != nil || got.Versioned {
		t.Errorf("file-backed read = (%+v, %v), want no version", got, err)
	}
}
