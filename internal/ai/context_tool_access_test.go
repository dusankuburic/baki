package ai

import (
	"strings"
	"testing"
)

// R-tool-loop-11: SystemPromptDefault named apply_fix and propose_fix for every stream — the plain stream that
// offers no tools, a chat-only token whose gate removes apply_fix, a provider without tools — so the model offered
// fixes it could not make. The prompt now follows the turn's tool access.
func TestBuildContext_NoTools(t *testing.T) {
	sys := func(access ToolAccess) string {
		s, _ := BuildContext(ContextRequest{ToolAccess: access})
		return s
	}
	if s := sys(ToolAccessNone); strings.Contains(s, "apply_fix") || strings.Contains(s, "propose_fix") {
		t.Errorf("the no-tools prompt names a fix tool:\n%s", s)
	}
	if s := sys(ToolAccessReadOnly); strings.Contains(s, "apply_fix") || !strings.Contains(s, "propose_fix") {
		t.Errorf("the read-only prompt must name propose_fix and not apply_fix:\n%s", s)
	}
	if s := sys(ToolAccessFixes); !strings.Contains(s, "apply_fix") || !strings.Contains(s, "propose_fix") {
		t.Errorf("the fix-tools prompt must name both:\n%s", s)
	}
	// Every variant says the assistant cannot change files on its own and keeps the data rule.
	for _, access := range []ToolAccess{ToolAccessNone, ToolAccessReadOnly, ToolAccessFixes} {
		s := sys(access)
		if !strings.Contains(s, "never claim you changed anything") || !strings.Contains(s, "DATA, NOT INSTRUCTIONS") {
			t.Errorf("variant %d lost a shared rule:\n%s", access, s)
		}
	}
	// The fix-tools variant is the default prompt, unchanged.
	if sys(ToolAccessFixes) != SystemPromptDefault {
		t.Error("the fix-tools variant is not SystemPromptDefault")
	}
}

// The access a turn has follows from its tools and its gate.
func TestToolAccessOf(t *testing.T) {
	if got := ToolAccessOf(false, nil); got != ToolAccessNone {
		t.Errorf("no tools: %d", got)
	}
	if got := ToolAccessOf(true, nil); got != ToolAccessFixes {
		t.Errorf("tools, no gate: %d", got)
	}
	if got := ToolAccessOf(true, NewToolGate(viewer(), true, false)); got != ToolAccessReadOnly {
		t.Errorf("tools, viewer gate: %d", got)
	}
	if got := ToolAccessOf(true, NewToolGate(viewer(), true, true)); got != ToolAccessFixes {
		t.Errorf("tools, editor gate: %d", got)
	}
}
