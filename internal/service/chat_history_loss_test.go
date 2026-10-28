package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"

	"pad-core/models"
)

// flakyConvStore fails LoadConversation on demand and records every save, so a
// test can assert what a failed load causes to be written.
type flakyConvStore struct {
	mu       sync.Mutex
	stored   []storageif.ChatMessage
	failLoad error
	saves    int
	version  int64
}

func (f *flakyConvStore) SaveConversation(_ context.Context, _, _ string, m []storageif.ChatMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	f.stored = append([]storageif.ChatMessage(nil), m...)
	return nil
}

func (f *flakyConvStore) LoadConversation(_ context.Context, _, _ string) ([]storageif.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failLoad != nil {
		return nil, f.failLoad
	}
	return append([]storageif.ChatMessage(nil), f.stored...), nil
}

func (f *flakyConvStore) DeleteConversation(_ context.Context, _, _ string) error { return nil }

// The versioned methods keep one version that every save moves, so the compare-and-swap path of the service is
// exercised against the same failing load.
func (f *flakyConvStore) LoadConversationVersioned(ctx context.Context, flowID, scope, owner string) ([]storageif.ChatMessage, int64, error) {
	msgs, err := f.LoadConversation(ctx, flowID, scope)
	f.mu.Lock()
	defer f.mu.Unlock()
	return msgs, f.version, err
}

func (f *flakyConvStore) SaveConversationIf(_ context.Context, _, _, _ string, expected int64, m []storageif.ChatMessage) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if expected != f.version {
		return f.version, storageif.ErrConversationConflict
	}
	f.saves++
	f.version++
	f.stored = append([]storageif.ChatMessage(nil), m...)
	return f.version, nil
}

func (f *flakyConvStore) DeleteConversationIf(_ context.Context, _, _, _ string, _ int64, _ bool) error {
	return nil
}

func (f *flakyConvStore) ListConversations(context.Context, string, string, *storageif.PageCursor, int) ([]storageif.ConversationSummary, error) {
	return nil, nil
}

func (f *flakyConvStore) SetConversationTitle(context.Context, string, string, string, string) (int64, error) {
	return 0, nil
}

func (f *flakyConvStore) IsSharedConversation(context.Context, string, string) (bool, error) {
	return false, nil
}

func (f *flakyConvStore) SaveUsageMetric(_ context.Context, _ *storageif.UsageMetric) error {
	return nil
}
func (f *flakyConvStore) GetDailyUsage(_ context.Context, _, _ string) (float64, error) {
	return 0, nil
}

// The caller has no settings row of their own and belongs to no organisation.
func (f *flakyConvStore) LoadUserSettings(context.Context, string) (*storageif.AppSettings, error) {
	return nil, nil
}
func (f *flakyConvStore) OrgMemberRole(context.Context, string, string) (auth.Role, bool, error) {
	return "", false, nil
}

func (f *flakyConvStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stored)
}

// TestChatHistory_FailedLoadDoesNotWipeTheStoredConversation is the regression gate for
// silent, permanent user data loss: reconstructHistory swallowed the load error and
// returned nil, reconstructAndPersistUserTurn assigned it to req.Messages, and
// persistInitialUserTurn wrote `req.Messages + [userMsg]` through SaveConversation — a
// FULL REPLACE — so one transient DB error, RLS denial or JSON unmarshal failure
// permanently deleted the user's entire chat history behind a WARN nobody reads. No
// attacker or unusual state required: anyone with viewer access, on any backend hiccup.
func TestChatHistory_FailedLoadDoesNotWipeTheStoredConversation(t *testing.T) {
	store := &flakyConvStore{}
	// Seed a real conversation.
	seed := make([]storageif.ChatMessage, 0, 10)
	for i := 0; i < 10; i++ {
		seed = append(seed, storageif.ChatMessage{Role: "user", Content: "message"})
	}
	if err := store.SaveConversation(context.Background(), "flow-1", "flow", seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if store.count() != 10 {
		t.Fatalf("fixture rot: seeded %d messages", store.count())
	}

	svc := &ChatService{backend: store, mode: config.ModeCloud}
	doc := &models.FlowDocument{ID: "flow-1"}

	// The next load fails, exactly as a transient DB error would.
	store.failLoad = errors.New("simulated: connection reset by peer")

	req := models.ChatRequest{UserMessage: "what does this flow do?"}
	svc.reconstructAndPersistUserTurn(context.Background(), doc, &req, "flow")

	if got := store.count(); got < 10 {
		t.Errorf("a failed history load destroyed the stored conversation: %d messages remain, "+
			"was 10. SaveConversation is a full replace, so persisting an empty reconstruction "+
			"deletes everything the user had.", got)
	}
}

// TestChatHistory_SuccessfulLoadStillAppends keeps the normal path intact — a
// guard that refuses to persist would "fix" the data loss by breaking the
// feature.
func TestChatHistory_SuccessfulLoadStillAppends(t *testing.T) {
	store := &flakyConvStore{}
	seed := []storageif.ChatMessage{{Role: "user", Content: "first"}}
	if err := store.SaveConversation(context.Background(), "flow-1", "flow", seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := &ChatService{backend: store, mode: config.ModeCloud}
	doc := &models.FlowDocument{ID: "flow-1"}

	req := models.ChatRequest{UserMessage: "second"}
	svc.reconstructAndPersistUserTurn(context.Background(), doc, &req, "flow")

	if got := store.count(); got != 2 {
		t.Errorf("the healthy path stored %d messages, want 2 (the seeded one plus the new turn)", got)
	}
}
