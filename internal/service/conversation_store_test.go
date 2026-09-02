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

// TestConversationStore_ToolTrailRoundTrip proves the transparency fields
// (ToolCalls + FixProposal snapshot) survive BOTH persistence paths: the
// filesystem file (models JSON directly) and the storage-backend bridge
// (models ↔ interfaces field-by-field conversion), which is where a forgotten
// field would silently strip the trail in cloud mode only.
func TestConversationStore_ToolTrailRoundTrip(t *testing.T) {
	msgs := []models.ChatMessage{{
		ID: "1", Role: "assistant", Content: "fixed it",
		ToolCalls: []models.ToolCallRecord{
			{Name: "list_findings", Label: "Listing findings", Ok: true, DurationMs: 12, Summary: "3 findings"},
			{Name: "apply_fix", Label: "Applying fix", Ok: false, DurationMs: 60002, Summary: "error: no decision"},
		},
		FixProposal: &models.FixProposalSnapshot{
			ProposalID: "p1", RuleID: "unhandled-error", FixType: "wrap-error-handler",
			BlockLabel: "Main/Xavier", Line: 4, Summary: "wrap in ON BLOCK ERROR",
			Status: "timeout", Message: "no decision within 60s",
		},
		FixProposals: []models.FixProposalSnapshot{
			{
				ProposalID: "p2", RuleID: "unhandled-error", FixType: "wrap-error-handler",
				BlockLabel: "Main/Xavier", Line: 4, Summary: "wrap", Status: "applied",
				Items: []models.FixItemSnapshot{
					{RuleID: "unhandled-error", FixType: "wrap-error-handler", BlockLabel: "Call API", Line: 3, Summary: "s1", Status: "applied"},
					{RuleID: "missing-retry", FixType: "wrap-in-retry", BlockLabel: "Sync API", Line: 8, Summary: "s2", Status: "applied-unresolved", Message: "still appears"},
				},
			},
			{ProposalID: "p3", RuleID: "missing-retry", FixType: "wrap-in-retry", BlockLabel: "Sync", Line: 9, Summary: "x", Status: "declined"},
		},
	}}

	// Path 1: filesystem (local mode) — models serialize verbatim.
	svc := &ChatService{configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	if err := svc.SaveConversation(context.Background(), doc, "flow", msgs); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}
	got, err := svc.GetConversation(context.Background(), doc, "flow")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	assertToolTrail(t, got, "filesystem")

	// Path 2: the models↔storage bridge (cloud mode) — field-by-field copy.
	stored := toStorageMessages(msgs)
	back := toModelMessages(stored)
	assertToolTrail(t, back, "bridge")
}

func assertToolTrail(t *testing.T, got []models.ChatMessage, path string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("[%s] expected 1 message, got %d", path, len(got))
	}
	m := got[0]
	if len(m.ToolCalls) != 2 {
		t.Fatalf("[%s] ToolCalls lost: %+v", path, m.ToolCalls)
	}
	if m.ToolCalls[1].Name != "apply_fix" || m.ToolCalls[1].Ok || m.ToolCalls[1].DurationMs != 60002 {
		t.Errorf("[%s] ToolCalls[1] mismatch: %+v", path, m.ToolCalls[1])
	}
	if m.FixProposal == nil {
		t.Fatalf("[%s] FixProposal snapshot lost", path)
	}
	if m.FixProposal.Status != "timeout" || m.FixProposal.FixType != "wrap-error-handler" || m.FixProposal.Message != "no decision within 60s" {
		t.Errorf("[%s] FixProposal mismatch: %+v", path, m.FixProposal)
	}
	// Sequential + batch proposal records survive with per-item outcomes.
	if len(m.FixProposals) != 2 {
		t.Fatalf("[%s] FixProposals lost: %+v", path, m.FixProposals)
	}
	batch := m.FixProposals[0]
	if batch.ProposalID != "p2" || len(batch.Items) != 2 {
		t.Errorf("[%s] batch record mismatch: %+v", path, batch)
	}
	if batch.Items[1].Status != "applied-unresolved" || batch.Items[1].Message != "still appears" {
		t.Errorf("[%s] batch item outcome lost: %+v", path, batch.Items[1])
	}
	if m.FixProposals[1].Status != "declined" {
		t.Errorf("[%s] second sequential proposal lost: %+v", path, m.FixProposals[1])
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

// TestReconstructHistory covers the server-side history-reconstruction path
// (C-conversation): when the client omits Messages the backend loads the prior
// conversation keyed by contextBlockId (or "flow"), and a store miss/error
// degrades to empty history instead of failing.
func TestReconstructHistory(t *testing.T) {
	dir := t.TempDir()
	svc := &ChatService{configDir: dir}
	doc := &models.FlowDocument{ID: "flow-1"}
	stored := []models.ChatMessage{
		{ID: "u1", Role: "user", Content: "previous question"},
		{ID: "a1", Role: "assistant", Content: "previous answer"},
	}
	// Persist under the default "flow" key (no contextBlockId).
	if err := svc.SaveConversation(context.Background(), doc, "flow", stored); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}

	// No ContextBlockID → key falls back to "flow" → loads the stored history.
	got := svc.reconstructHistory(context.Background(), doc, models.ChatRequest{})
	if len(got) != 2 || got[0].Content != "previous question" {
		t.Fatalf("expected stored history, got %+v", got)
	}

	// A contextBlockId keys its own conversation slot; absent → empty (not error).
	gotBlock := svc.reconstructHistory(context.Background(), doc, models.ChatRequest{ContextBlockID: "block-7"})
	if len(gotBlock) != 0 {
		t.Fatalf("expected empty history for an unknown contextBlockId, got %d msgs", len(gotBlock))
	}

	// nil doc → nil (no panic, no store access).
	if got := svc.reconstructHistory(context.Background(), nil, models.ChatRequest{}); got != nil {
		t.Errorf("expected nil for nil doc, got %+v", got)
	}

	// Persist a block-scoped conversation and confirm it loads by that key.
	blockMsgs := []models.ChatMessage{{ID: "b1", Role: "user", Content: "block question"}}
	if err := svc.SaveConversation(context.Background(), doc, "block-7", blockMsgs); err != nil {
		t.Fatalf("SaveConversation block: %v", err)
	}
	gotBlock2 := svc.reconstructHistory(context.Background(), doc, models.ChatRequest{ContextBlockID: "block-7"})
	if len(gotBlock2) != 1 || gotBlock2[0].Content != "block question" {
		t.Fatalf("expected block-scoped history, got %+v", gotBlock2)
	}
}
