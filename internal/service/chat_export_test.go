package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/config"
	"pad-core/models"
)

// ExportConversation reads the conversation through the (mode-aware) GetConversation
// and writes a human-readable transcript. It must reject an unsafe export path
// before doing any work.
func TestChatService_ExportConversation(t *testing.T) {
	svc := &ChatService{mode: config.ModeLocal, configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	ctx := context.Background()

	msgs := []models.ChatMessage{
		{ID: "1", Role: "user", Content: "hello world", Timestamp: time.Now().UTC()},
		{ID: "2", Role: "assistant", Content: "hi there"},
	}
	if err := svc.SaveConversation(ctx, doc, "claude", msgs); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}

	out := filepath.Join(t.TempDir(), "export.md")
	if err := svc.ExportConversation(ctx, doc, "claude", out); err != nil {
		t.Fatalf("ExportConversation: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	content := string(data)
	for _, want := range []string{"hello world", "hi there", "user", "assistant"} {
		if !strings.Contains(content, want) {
			t.Errorf("export missing %q:\n%s", want, content)
		}
	}
}

// A path containing a NUL byte is rejected by validateUserPath, so no file is
// written and the unsafe path never reaches os.WriteFile.
func TestChatService_ExportConversation_RejectsUnsafePath(t *testing.T) {
	svc := &ChatService{mode: config.ModeLocal, configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}

	if err := svc.ExportConversation(context.Background(), doc, "claude", "bad\x00path"); err == nil {
		t.Error("expected error for path containing a NUL byte")
	}
}
