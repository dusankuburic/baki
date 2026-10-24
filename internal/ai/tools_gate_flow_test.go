package ai

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
)

// R-tool-loop-11: the gate looked at the token's scope and whether an applier was wired, never at the stream's
// flow permission. A chat stream needs only ChatUse (a viewer), so a viewer was offered apply_fix and apply_fixes,
// the approval card was emitted, and only the apply step refused — after the human was asked.

func viewer() *auth.Actor {
	return &auth.Actor{Kind: auth.ActorUser, UserID: "u-v", Role: auth.RoleMember}
}

func TestToolGate_ViewerIsRefusedTheFixTools(t *testing.T) {
	gate := NewToolGate(viewer(), true, false)
	for _, spec := range ToolSpecs() {
		want := ""
		if spec.RequiresApproval {
			want = ToolRefusedFlowPermission
		}
		if got := gate(spec); got != want {
			t.Errorf("%s: refusal %q, want %q", spec.Def.Name, got, want)
		}
	}
	names := strings.Join(toolNames(ToolDefinitionsFor(gate)), ",")
	if strings.Contains(names, "apply_fix") || !strings.Contains(names, "propose_fix") {
		t.Errorf("a viewer is offered %s, want the read-only tools and propose_fix", names)
	}
	// The preamble of the marker instructions names apply_fix only when it is offered (the tools' own
	// descriptions are fixed text and may mention one another).
	instr := ToolPromptInstructionsFor(gate)
	if preamble := instr[:strings.Index(instr, "Available tools")]; strings.Contains(preamble, "apply_fix") {
		t.Errorf("the marker preamble names apply_fix for a viewer:\n%s", preamble)
	}
}

// A chat-only token is refused for its scope first: the reason names what the caller would have to change.
func TestToolGate_ScopeIsReportedBeforeFlowPermission(t *testing.T) {
	spec, _ := ToolSpecOf("apply_fix")
	if got := NewToolGate(scopedTokenActor(auth.ScopeChat), true, false)(spec); got != ToolRefusedScope {
		t.Errorf("chat-only token without edit rights: %q, want %q", got, ToolRefusedScope)
	}
	if got := NewToolGate(viewer(), false, false)(spec); got != ToolRefusedApprovalUnavailable {
		t.Errorf("no applier: %q, want %q", got, ToolRefusedApprovalUnavailable)
	}
	if got := NewToolGate(viewer(), true, true)(spec); got != "" {
		t.Errorf("an editor: %q, want allowed", got)
	}
}

// ExecuteTool asks the same gate: a viewer's apply_fix never reaches the handler, so no card is shown.
func TestExecuteTool_ViewerApplyFixNeverReachesTheHandler(t *testing.T) {
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc(), Fixes: failingFixApplier{t}, Allow: NewToolGate(viewer(), true, false)}
	for _, name := range []string{"apply_fix", "apply_fixes"} {
		out := ExecuteTool(name, []byte(`{"blockId":"b1","ruleId":"r","targets":[{"blockId":"b1","ruleId":"r"}]}`), tctx)
		if !strings.Contains(out, "not available in this session") {
			t.Errorf("%s: %q, want the refusal", name, out)
		}
	}
}
