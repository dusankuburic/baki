package ai

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/models"
)

// evictReference is the original O(n²) eviction algorithm, kept in the test as
// an oracle to prove the optimized evictIfNeeded selects the identical cut.
func evictReference(messages []models.ChatMessage) []models.ChatMessage {
	if len(messages) <= maxMessagesPerConv {
		return messages
	}
	data, _ := json.Marshal(messages)
	if len(data) <= maxConversationBytes {
		return messages
	}
	cut := 0
	for cut < len(messages)-2 {
		cut += 2
		trimmed, _ := json.Marshal(messages[cut:])
		if len(trimmed) <= maxConversationBytes && len(messages[cut:]) <= maxMessagesPerConv {
			break
		}
	}
	return messages[cut:]
}

// TestEvictIfNeeded_MatchesReference verifies the optimized single-pass eviction
// produces byte-identical cuts to the original brute-force version across
// conversations whose per-message sizes vary (so suffix sums are non-uniform).
func TestEvictIfNeeded_MatchesReference(t *testing.T) {
	mk := func(n int, contentLen func(i int) int) []models.ChatMessage {
		msgs := make([]models.ChatMessage, n)
		for i := range msgs {
			msgs[i] = models.ChatMessage{
				ID:      fmt.Sprintf("m%04d", i),
				Role:    "user",
				Content: strings.Repeat("x", contentLen(i)),
			}
		}
		return msgs
	}
	cases := map[string][]models.ChatMessage{
		"uniform_large": mk(60, func(int) int { return 25_000 }),
		"growing":       mk(80, func(i int) int { return 1_000 + i*500 }),
		"shrinking":     mk(80, func(i int) int { return 60_000 - i*500 }),
		"mixed_small_big": mk(120, func(i int) int {
			if i%3 == 0 {
				return 40_000
			}
			return 500
		}),
		"under_limit": mk(10, func(int) int { return 100 }),
	}
	for name, msgs := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := evictIfNeeded(msgs)
			if err != nil {
				t.Fatalf("evictIfNeeded: %v", err)
			}
			want := evictReference(msgs)
			if len(got) != len(want) {
				t.Fatalf("cut mismatch: optimized kept %d, reference kept %d", len(got), len(want))
			}
			for i := range got {
				if got[i].ID != want[i].ID {
					t.Fatalf("message %d mismatch: %s vs %s", i, got[i].ID, want[i].ID)
				}
			}
		})
	}
}

func BenchmarkEvictIfNeeded(b *testing.B) {
	msgs := make([]models.ChatMessage, 200)
	for i := range msgs {
		msgs[i] = models.ChatMessage{ID: fmt.Sprintf("m%04d", i), Role: "user", Content: strings.Repeat("x", 20_000)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = evictIfNeeded(msgs)
	}
}

// ---- FlowKey ----------------------------------------------------------------

func TestFlowKey_Deterministic(t *testing.T) {
	a := FlowKey("/my/flow/main.txt")
	b := FlowKey("/my/flow/main.txt")
	if a != b {
		t.Errorf("FlowKey is not deterministic: %q != %q", a, b)
	}
}

func TestFlowKey_DifferentPaths(t *testing.T) {
	a := FlowKey("/flow/a.txt")
	b := FlowKey("/flow/b.txt")
	if a == b {
		t.Errorf("different paths produced the same FlowKey: %q", a)
	}
}

func TestFlowKey_Format(t *testing.T) {
	key := FlowKey("/any/path")
	// Should be 16 lowercase hex chars (8 bytes → 16 hex chars).
	if len(key) != 16 {
		t.Errorf("FlowKey length = %d, want 16; key = %q", len(key), key)
	}
	for _, c := range key {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("FlowKey contains non-hex char %q: %q", c, key)
		}
	}
}

// ---- validateScope ----------------------------------------------------------

func TestValidateScope_Valid(t *testing.T) {
	cases := []string{"flow", "main", "my-scope", "scope_1", "abc123"}
	for _, s := range cases {
		if err := validateScope(s); err != nil {
			t.Errorf("validateScope(%q) returned unexpected error: %v", s, err)
		}
	}
}

func TestValidateScope_Invalid(t *testing.T) {
	cases := []string{"has space", "a/b", "a.b", "../etc/passwd", ""}
	for _, s := range cases {
		// "" doesn't match the regex, but it also isn't "flow" —
		// so both branches should reject it.
		if s == "" {
			// empty string: falls through to regex which won't match → error expected
		}
		if err := validateScope(s); err == nil {
			t.Errorf("validateScope(%q) expected error, got nil", s)
		}
	}
}

// ---- SaveConversation / LoadConversation ------------------------------------

func makeMessages(n int) []models.ChatMessage {
	msgs := make([]models.ChatMessage, n)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = models.ChatMessage{
			ID:        "msg-" + string(rune('a'+i)),
			Role:      role,
			Content:   "content " + string(rune('a'+i)),
			Timestamp: time.Now(),
		}
	}
	return msgs
}

func TestSaveAndLoadConversation_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	msgs := makeMessages(4)

	if err := SaveConversation(dir, "/flow/test.txt", "flow", msgs); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}

	conv, err := LoadConversation(dir, "/flow/test.txt", "flow")
	if err != nil {
		t.Fatalf("LoadConversation: %v", err)
	}

	if len(conv.Messages) != len(msgs) {
		t.Fatalf("loaded %d messages, want %d", len(conv.Messages), len(msgs))
	}
	for i, m := range msgs {
		if conv.Messages[i].ID != m.ID {
			t.Errorf("msg[%d].ID = %q, want %q", i, conv.Messages[i].ID, m.ID)
		}
		if conv.Messages[i].Content != m.Content {
			t.Errorf("msg[%d].Content = %q, want %q", i, conv.Messages[i].Content, m.Content)
		}
	}
	if conv.Scope != "flow" {
		t.Errorf("Scope = %q, want %q", conv.Scope, "flow")
	}
	if conv.FlowKey != FlowKey("/flow/test.txt") {
		t.Errorf("FlowKey mismatch: got %q, want %q", conv.FlowKey, FlowKey("/flow/test.txt"))
	}
}

func TestLoadConversation_MissingFile_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()

	conv, err := LoadConversation(dir, "/ghost/file.txt", "flow")
	if err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
	if conv == nil {
		t.Fatal("expected non-nil ConversationFile for missing file")
	}
	if len(conv.Messages) != 0 {
		t.Errorf("expected empty messages for missing file, got %d", len(conv.Messages))
	}
}

func TestLoadConversation_CorruptJSON_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	// Write a corrupt file at the expected path.
	flowDir := filepath.Join(ConversationDir(dir), FlowKey("/my/flow.txt"))
	if err := os.MkdirAll(flowDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowDir, "flow.json"), []byte("{bad json"), 0600); err != nil {
		t.Fatal(err)
	}

	conv, err := LoadConversation(dir, "/my/flow.txt", "flow")
	if err != nil {
		t.Fatalf("expected graceful recovery from corrupt JSON, got: %v", err)
	}
	if len(conv.Messages) != 0 {
		t.Errorf("expected empty messages after corrupt recovery, got %d", len(conv.Messages))
	}
}

func TestLoadConversation_FutureVersion_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	// Save a conversation and then manually bump its version.
	if err := SaveConversation(dir, "/flow/v.txt", "flow", makeMessages(2)); err != nil {
		t.Fatal(err)
	}
	convFile := filepath.Join(ConversationDir(dir), FlowKey("/flow/v.txt"), "flow.json")
	data, err := os.ReadFile(convFile)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the version field — MarshalIndent adds a space after the colon.
	raw := string(data)
	patched := strings.ReplaceAll(raw, `"version": 1`, `"version": 9999`)
	if patched == raw {
		// Fallback in case the serialisation format differs.
		patched = strings.ReplaceAll(raw, `"version":1`, `"version":9999`)
	}
	if err := os.WriteFile(convFile, []byte(patched), 0600); err != nil {
		t.Fatal(err)
	}

	conv, err := LoadConversation(dir, "/flow/v.txt", "flow")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(conv.Messages) != 0 {
		t.Errorf("expected empty messages for future version, got %d", len(conv.Messages))
	}
}

func TestSaveConversation_InvalidScope_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := SaveConversation(dir, "/flow/f.txt", "bad/scope", nil); err == nil {
		t.Fatal("expected error for invalid scope, got nil")
	}
}

// ---- ClearAllConversations --------------------------------------------------

func TestClearAllConversations_RemovesDirectory(t *testing.T) {
	dir := t.TempDir()

	// Populate a conversation so the directory tree exists.
	if err := SaveConversation(dir, "/flow/a.txt", "flow", makeMessages(2)); err != nil {
		t.Fatal(err)
	}
	baseDir := ConversationDir(dir)
	if _, err := os.Stat(baseDir); err != nil {
		t.Fatalf("conversation dir should exist after save: %v", err)
	}

	if err := ClearAllConversations(dir); err != nil {
		t.Fatalf("ClearAllConversations: %v", err)
	}
	if _, err := os.Stat(baseDir); !os.IsNotExist(err) {
		t.Error("expected conversation dir to be removed after clear")
	}
}

func TestClearAllConversations_NoOp_WhenMissing(t *testing.T) {
	dir := t.TempDir()
	// Calling on a dir with no conversations sub-directory should not error.
	if err := ClearAllConversations(dir); err != nil {
		t.Fatalf("ClearAllConversations on missing dir: %v", err)
	}
}

// ---- evictIfNeeded ----------------------------------------------------------

func TestEvictIfNeeded_UnderLimit_NoChange(t *testing.T) {
	msgs := makeMessages(10) // well under maxMessagesPerConv (50)
	got, err := evictIfNeeded(msgs)
	if err != nil {
		t.Fatalf("evictIfNeeded: %v", err)
	}
	if len(got) != len(msgs) {
		t.Errorf("expected %d messages unchanged, got %d", len(msgs), len(got))
	}
}

func TestEvictIfNeeded_OverCount_Evicts(t *testing.T) {
	// evictIfNeeded requires BOTH count > maxMessagesPerConv AND size > maxConversationBytes
	// to actually trim. Build messages large enough to exceed the 1MB threshold.
	largeContent := strings.Repeat("x", 25_000) // ~25KB each
	n := maxMessagesPerConv + 10                // 60
	msgs := make([]models.ChatMessage, n)
	for i := range msgs {
		msgs[i] = models.ChatMessage{
			ID:      "msg-" + string(rune('a'+i%26)),
			Role:    "user",
			Content: largeContent,
		}
	}

	got, err := evictIfNeeded(msgs)
	if err != nil {
		t.Fatalf("evictIfNeeded: %v", err)
	}
	if len(got) >= len(msgs) {
		t.Errorf("expected fewer messages after eviction, got same (%d)", len(got))
	}
	// Must preserve the most recent message.
	last := got[len(got)-1]
	wantLast := msgs[len(msgs)-1]
	if last.ID != wantLast.ID {
		t.Errorf("last message ID = %q, want %q (most recent must be preserved)", last.ID, wantLast.ID)
	}
}

// ---- EvictOldConversations --------------------------------------------------

func TestEvictOldConversations_NoDirectory_NoError(t *testing.T) {
	dir := t.TempDir()
	// The conversations sub-directory doesn't exist yet.
	if err := EvictOldConversations(dir); err != nil {
		t.Fatalf("EvictOldConversations on missing dir: %v", err)
	}
}

func TestEvictOldConversations_UnderLimit_KeepsFiles(t *testing.T) {
	dir := t.TempDir()
	// Save a few small conversations — total well under 50MB.
	for _, path := range []string{"/flow/a.txt", "/flow/b.txt", "/flow/c.txt"} {
		if err := SaveConversation(dir, path, "flow", makeMessages(3)); err != nil {
			t.Fatalf("SaveConversation(%q): %v", path, err)
		}
	}

	if err := EvictOldConversations(dir); err != nil {
		t.Fatalf("EvictOldConversations: %v", err)
	}

	// All three conversations should still be loadable.
	for _, path := range []string{"/flow/a.txt", "/flow/b.txt", "/flow/c.txt"} {
		conv, err := LoadConversation(dir, path, "flow")
		if err != nil {
			t.Fatalf("LoadConversation(%q) after evict: %v", path, err)
		}
		if len(conv.Messages) == 0 {
			t.Errorf("expected messages for %q to survive eviction", path)
		}
	}
}
