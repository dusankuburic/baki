package service

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"

	"pad-core/models"
)

func TestConvFilePath_RejectsTraversal(t *testing.T) {
	bad := []struct{ scope, flow string }{
		{"../etc", "f1"},
		{"flow", "../../secret"},
		{"a/b", "f1"},
		{"..", "f1"},
		{"", "f1"},
		{"flow", ""},
	}
	for _, c := range bad {
		if _, err := convFilePath("/cfg", c.scope, c.flow); err == nil {
			t.Errorf("convFilePath(scope=%q, flow=%q) should be rejected", c.scope, c.flow)
		}
	}
	if _, err := convFilePath("/cfg", "flow", "f1"); err != nil {
		t.Errorf("valid components rejected: %v", err)
	}
}

func TestSaveConversation_RejectsBadScope(t *testing.T) {
	svc := &ChatService{configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "f1"}
	if err := svc.SaveConversation(context.Background(), doc, "../escape", nil); err == nil {
		t.Error("SaveConversation with traversal scope should error")
	}
}

func TestSaveGetConversation_RoundTripAndPerms(t *testing.T) {
	dir := t.TempDir()
	svc := &ChatService{configDir: dir}
	doc := &models.FlowDocument{ID: "flow-1"}
	msgs := []models.ChatMessage{
		{ID: "1", Role: "user", Content: "hello"},
		{ID: "2", Role: "assistant", Content: "hi"},
	}

	if err := svc.SaveConversation(context.Background(), doc, "flow", msgs); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}

	got, err := svc.GetConversation(context.Background(), doc, "flow")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(got) != 2 || got[0].Content != "hello" || got[1].Content != "hi" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// The file must be written 0600 (not world-readable). Skip on Windows where
	// Unix perms don't apply.
	if runtime.GOOS != "windows" {
		path, _ := convFilePath(dir, "flow", "flow-1")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("conversation file perm = %o, want 0600", perm)
		}
	}

	// Clearing removes it; a subsequent read returns an empty slice, not an error.
	if err := svc.ClearConversation(context.Background(), doc, "flow"); err != nil {
		t.Fatalf("ClearConversation: %v", err)
	}
	if got, err := svc.GetConversation(context.Background(), doc, "flow"); err != nil || len(got) != 0 {
		t.Errorf("after clear: got %d msgs, err %v", len(got), err)
	}
}

func TestEvictConvMessages(t *testing.T) {
	// Under the count limit → untouched.
	small := make([]models.ChatMessage, 10)
	if out := evictConvMessages(small); len(out) != 10 {
		t.Errorf("small history trimmed: got %d", len(out))
	}

	// Over the count limit AND over the byte budget → trimmed, retaining the
	// most recent turns (and never below the floor).
	big := make([]models.ChatMessage, 80)
	for i := range big {
		big[i] = models.ChatMessage{ID: "m", Role: "user", Content: strings.Repeat("x", 20000)}
	}
	big[len(big)-1].Content = "LAST"
	out := evictConvMessages(big)
	if len(out) >= len(big) {
		t.Errorf("oversized history not trimmed: %d", len(out))
	}
	if len(out) < minRetainedConvMsgs {
		t.Errorf("trimmed below floor: %d < %d", len(out), minRetainedConvMsgs)
	}
	if out[len(out)-1].Content != "LAST" {
		t.Error("eviction dropped the most recent message")
	}
}
