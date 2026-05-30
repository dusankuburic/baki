package database_test

import (
	"context"
	"os"
	"testing"
	"time"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_Contract runs the cross-backend contract suite against
// Postgres so the two storage backends cannot quietly diverge on return-
// shape semantics (nil vs empty slice, ErrNotFound vs nil, etc.). The
// same suite runs against the filesystem backend in
// `filesystem/local_storage_test.go::TestLocalStorageBackend_Contract`.
func TestPostgres_Contract(t *testing.T) {
	b := openTestDB(t)
	contract.RunSuite(t, b)
}

// openTestDB connects using DATABASE_URL env var and skips if not set.
func openTestDB(t *testing.T) *database.PostgresStorageBackend {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping PostgreSQL integration tests")
	}
	b, err := database.New(database.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func TestPostgres_Ping(t *testing.T) {
	b := openTestDB(t)
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestPostgres_Flow_SaveLoadDelete(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	flow := &interfaces.FlowDocument{
		ID:          "test-flow-1",
		Name:        "Test Flow",
		Description: "Integration test flow",
		Content:     []byte(`{"subflows":[]}`),
		Metadata: interfaces.FlowMetadata{
			BlockCount:   5,
			SubflowCount: 1,
			ParsedAt:     time.Now().UTC(),
		},
	}

	// Save
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	t.Cleanup(func() { b.DeleteFlow(ctx, flow.ID) })

	// Load
	got, err := b.LoadFlow(ctx, flow.ID)
	if err != nil {
		t.Fatalf("LoadFlow: %v", err)
	}
	if got.Name != flow.Name {
		t.Errorf("Name: want %q, got %q", flow.Name, got.Name)
	}

	// Update
	flow.Name = "Updated Flow"
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow (update): %v", err)
	}
	got, _ = b.LoadFlow(ctx, flow.ID)
	if got.Name != "Updated Flow" {
		t.Errorf("Name after update: want %q, got %q", "Updated Flow", got.Name)
	}

	// Delete
	if err := b.DeleteFlow(ctx, flow.ID); err != nil {
		t.Fatalf("DeleteFlow: %v", err)
	}
	if _, err := b.LoadFlow(ctx, flow.ID); err == nil {
		t.Error("expected error loading deleted flow, got nil")
	}
}

func TestPostgres_ListFlows(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	ids := []string{"list-flow-a", "list-flow-b"}
	for _, id := range ids {
		f := &interfaces.FlowDocument{ID: id, Name: id, Content: []byte("{}")}
		if err := b.SaveFlow(ctx, f); err != nil {
			t.Fatalf("SaveFlow %q: %v", id, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			b.DeleteFlow(ctx, id)
		}
	})

	list, err := b.ListFlows(ctx, interfaces.FlowFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	found := 0
	for _, f := range list {
		for _, id := range ids {
			if f.ID == id {
				found++
			}
		}
	}
	if found != len(ids) {
		t.Errorf("expected %d flows in list, found %d", len(ids), found)
	}
}

func TestPostgres_Settings_SaveLoad(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	settings := &interfaces.AppSettings{
		Version: 2,
		General: interfaces.GeneralSettings{
			FirstRunCompleted: true,
			LastUsedVersion:   "1.2.3",
		},
	}

	if err := b.SaveSettings(ctx, settings); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	got, err := b.LoadSettings(ctx)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.Version != 2 {
		t.Errorf("Version: want 2, got %d", got.Version)
	}
	if !got.General.FirstRunCompleted {
		t.Error("FirstRunCompleted should be true")
	}
}

func TestPostgres_Conversation_SaveLoad(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	msgs := []interfaces.ChatMessage{
		{ID: "m1", Role: "user", Content: "Hello"},
		{ID: "m2", Role: "assistant", Content: "Hi there"},
	}

	if err := b.SaveConversation(ctx, "flow-1", "global", msgs); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}

	got, err := b.LoadConversation(ctx, "flow-1", "global")
	if err != nil {
		t.Fatalf("LoadConversation: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if got[0].Content != "Hello" {
		t.Errorf("msg[0]: want %q, got %q", "Hello", got[0].Content)
	}
}

func TestPostgres_LoadConversation_Empty(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	msgs, err := b.LoadConversation(ctx, "nonexistent-flow", "scope")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Contract: missing-conversation returns a non-nil empty slice, matching
	// the filesystem backend so callers don't need backend-specific nil checks.
	// (A nil return is reserved for error cases.)
	if msgs == nil {
		t.Errorf("expected empty slice for missing conversation, got nil")
	}
	if len(msgs) != 0 {
		t.Errorf("expected length 0, got %d", len(msgs))
	}
}

func TestPostgres_RefreshTokenRotation(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	user := "rt-test-user"
	t.Cleanup(func() { _ = b.RevokeUserRefreshTokens(ctx, user) })

	jti := "rt-jti-" + time.Now().Format("150405.000000000")

	// A freshly stored token is valid.
	if err := b.StoreRefreshToken(ctx, jti, user, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("StoreRefreshToken: %v", err)
	}
	if ok, err := b.IsRefreshTokenValid(ctx, jti); err != nil || !ok {
		t.Fatalf("expected valid token, got ok=%v err=%v", ok, err)
	}

	// Revoking (rotation) invalidates it.
	if err := b.RevokeRefreshToken(ctx, jti); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}
	if ok, _ := b.IsRefreshTokenValid(ctx, jti); ok {
		t.Fatal("expected revoked token to be invalid")
	}

	// Unknown jti is invalid but not an error.
	if ok, err := b.IsRefreshTokenValid(ctx, "does-not-exist"); err != nil || ok {
		t.Fatalf("expected unknown token invalid, got ok=%v err=%v", ok, err)
	}

	// Expired token is invalid.
	expJTI := jti + "-expired"
	if err := b.StoreRefreshToken(ctx, expJTI, user, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("StoreRefreshToken(expired): %v", err)
	}
	if ok, _ := b.IsRefreshTokenValid(ctx, expJTI); ok {
		t.Fatal("expected expired token to be invalid")
	}

	// RevokeUserRefreshTokens revokes every active token for the user.
	a, c := jti+"-a", jti+"-b"
	_ = b.StoreRefreshToken(ctx, a, user, time.Now().Add(time.Hour))
	_ = b.StoreRefreshToken(ctx, c, user, time.Now().Add(time.Hour))
	if err := b.RevokeUserRefreshTokens(ctx, user); err != nil {
		t.Fatalf("RevokeUserRefreshTokens: %v", err)
	}
	for _, j := range []string{a, c} {
		if ok, _ := b.IsRefreshTokenValid(ctx, j); ok {
			t.Fatalf("expected %s to be revoked after RevokeUserRefreshTokens", j)
		}
	}
}
