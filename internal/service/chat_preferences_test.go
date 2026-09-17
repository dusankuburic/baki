package service

import (
	"context"
	"testing"

	"pad-core/models"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// userPrefsBackend is the cloud backend with per-user settings rows.
type userPrefsBackend struct {
	*testutil.FakeBackend
	prefs map[string]*storageif.AppSettings
}

func (b *userPrefsBackend) LoadUserSettings(_ context.Context, userID string) (*storageif.AppSettings, error) {
	if p, ok := b.prefs[userID]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, storageif.ErrNotFound
}

func asUser(id string) context.Context {
	return auth.WithClaims(context.Background(), &auth.Claims{UserID: id})
}

// TestSaveConversation_CloudHonoursEachUsersHistoryToggle: "Save conversation
// history" had no reader, so turns were stored whatever it said. It is a
// per-user preference in cloud, so one user's choice must not affect another's.
func TestSaveConversation_CloudHonoursEachUsersHistoryToggle(t *testing.T) {
	backend := &userPrefsBackend{
		FakeBackend: testutil.NewFakeBackend(),
		prefs: map[string]*storageif.AppSettings{
			"alice": {AI: storageif.AISettings{SaveConversationHistory: false}},
			"bob":   {AI: storageif.AISettings{SaveConversationHistory: true}},
		},
	}
	svc := &ChatService{backend: backend, mode: config.ModeCloud, configDir: t.TempDir()}
	msgs := []models.ChatMessage{{ID: "1", Role: "user", Content: "hello"}}

	if err := svc.SaveConversation(asUser("alice"), &models.FlowDocument{ID: "flow-a"}, "thread-1", msgs); err != nil {
		t.Fatalf("SaveConversation alice: %v", err)
	}
	if got, _ := backend.LoadConversation(context.Background(), "flow-a", "thread-1"); len(got) != 0 {
		t.Errorf("history off: the turn must not be stored, got %d message(s)", len(got))
	}

	if err := svc.SaveConversation(asUser("bob"), &models.FlowDocument{ID: "flow-b"}, "thread-1", msgs); err != nil {
		t.Fatalf("SaveConversation bob: %v", err)
	}
	if got, _ := backend.LoadConversation(context.Background(), "flow-b", "thread-1"); len(got) != 1 {
		t.Errorf("history on: the turn must be stored, got %d message(s)", len(got))
	}
}

// TestSaveConversation_DesktopHonoursHistoryToggle: desktop reads the single
// local settings store.
func TestSaveConversation_DesktopHonoursHistoryToggle(t *testing.T) {
	off := &staticSettings{s: models.AppSettings{AI: models.AISettings{SaveConversationHistory: false}}}
	svc := &ChatService{settings: off, mode: config.ModeLocal, configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}

	if err := svc.SaveConversation(context.Background(), doc, "thread-1", []models.ChatMessage{{ID: "1", Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}
	got, err := svc.GetConversation(context.Background(), doc, "thread-1")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("history off: expected nothing stored, got %d message(s)", len(got))
	}
}

// TestSystemPromptSuffix_CloudUsesTheCallersInstructions: in cloud the custom
// instructions a user saves live in their own settings row; chat used to read
// only the server's settings file, so they never reached the prompt.
func TestSystemPromptSuffix_CloudUsesTheCallersInstructions(t *testing.T) {
	backend := &userPrefsBackend{
		FakeBackend: testutil.NewFakeBackend(),
		prefs: map[string]*storageif.AppSettings{
			"alice": {AI: storageif.AISettings{SystemPromptSuffix: "Answer in Serbian."}},
		},
	}
	deployment := &staticSettings{s: models.AppSettings{AI: models.AISettings{SystemPromptSuffix: "Deployment default."}}}
	svc := &ChatService{backend: backend, settings: deployment, mode: config.ModeCloud}
	req := models.ChatRequest{SystemPrompt: "Be brief."}

	if got, want := svc.systemPromptSuffix(asUser("alice"), req), "Answer in Serbian.\n\nBe brief."; got != want {
		t.Errorf("alice: got %q, want %q", got, want)
	}
	// No settings row for this caller: fall back to the deployment settings.
	if got, want := svc.systemPromptSuffix(asUser("carol"), req), "Deployment default.\n\nBe brief."; got != want {
		t.Errorf("caller without a settings row: got %q, want %q", got, want)
	}
	// No caller at all (background work): deployment settings.
	if got, want := svc.systemPromptSuffix(context.Background(), models.ChatRequest{}), "Deployment default."; got != want {
		t.Errorf("no caller: got %q, want %q", got, want)
	}
}
