package filesystem

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

func TestSafePathSegment(t *testing.T) {
	bad := []string{"", ".", "..", "../etc", "a/b", `a\b`, "..\\x", "foo/../bar", "a..b"}
	for _, s := range bad {
		if safePathSegment(s) {
			t.Errorf("safePathSegment(%q) = true, want false", s)
		}
	}
	good := []string{"flow-123", "uuid_abc-DEF", "abc123"}
	for _, s := range good {
		if !safePathSegment(s) {
			t.Errorf("safePathSegment(%q) = false, want true", s)
		}
	}
}

// TestConversationPaths_RejectTraversal is the L5 regression: the filesystem
// conversation methods must reject a traversal-bearing flowID/scope instead of
// building a path that escapes the data dir.
func TestConversationPaths_RejectTraversal(t *testing.T) {
	b, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	evil := "../../../etc/passwd"

	if err := b.SaveConversation(ctx, evil, "scope", nil); err == nil {
		t.Error("SaveConversation should reject a traversal flowID")
	}
	if err := b.SaveConversation(ctx, "flow", evil, nil); err == nil {
		t.Error("SaveConversation should reject a traversal scope")
	}
	if _, err := b.LoadConversation(ctx, evil, "scope"); err == nil {
		t.Error("LoadConversation should reject a traversal flowID")
	}
	if err := b.DeleteConversation(ctx, evil, "scope"); err == nil {
		t.Error("DeleteConversation should reject a traversal flowID")
	}
}

// TestTriageBaselinePaths_RejectTraversal covers the triage/baseline builders.
func TestTriageBaselinePaths_RejectTraversal(t *testing.T) {
	b, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	evil := "../../secret"

	if err := b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: evil, FindingKey: "k", Status: "accepted"}); err == nil || !strings.Contains(err.Error(), "invalid flow id") {
		t.Errorf("SetFindingStatus should reject a traversal flowID, got %v", err)
	}
	if _, err := b.ListFindingStatuses(ctx, evil); err == nil {
		t.Error("ListFindingStatuses should reject a traversal flowID")
	}
	if _, err := b.GetFlowBaseline(ctx, evil); err == nil {
		t.Error("GetFlowBaseline should reject a traversal flowID")
	}
	if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: evil}); err == nil {
		t.Error("SetFlowBaseline should reject a traversal flowID")
	}
	if err := b.ClearFlowBaseline(ctx, evil); err == nil {
		t.Error("ClearFlowBaseline should reject a traversal flowID")
	}
}
