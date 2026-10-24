package ai

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
)

var updateTools = flag.Bool("update-tools", false, "rewrite testdata/tools.golden")

// scopedTokenActor is a PAT restricted to scopes, the actor a chat stream started with such a token carries.
func scopedTokenActor(scopes ...string) *auth.Actor {
	return &auth.Actor{Kind: auth.ActorToken, UserID: "u-1", Scopes: scopes}
}

// failingFixApplier fails the test when a fix handler reaches it: a gated tool must be refused before its handler.
type failingFixApplier struct{ t *testing.T }

func (f failingFixApplier) ApplyFixWithApproval(context.Context, FixProposal) (string, error) {
	f.t.Error("apply_fix reached the applier despite the gate")
	return "", nil
}

func (f failingFixApplier) ApplyFixesWithApproval(context.Context, []FixProposal) (string, error) {
	f.t.Error("apply_fixes reached the applier despite the gate")
	return "", nil
}

func toolNames(defs []ToolDefinition) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}

// TestToolSpecs_Complete: every registered tool carries the metadata the loop, the gate and the approval card
// need, and the approval-gated ones say which scope and which preview kind they need.
func TestToolSpecs_Complete(t *testing.T) {
	specs := ToolSpecs()
	if len(specs) != 8 {
		t.Fatalf("expected 8 tools, got %d", len(specs))
	}
	approvalGated := 0
	for _, s := range specs {
		name := s.Def.Name
		if name == "" || s.Def.Description == "" || len(s.Def.InputSchema) == 0 {
			t.Errorf("tool %q: incomplete definition", name)
		}
		var js map[string]any
		if err := json.Unmarshal(s.Def.InputSchema, &js); err != nil {
			t.Errorf("tool %q: invalid JSON schema: %v", name, err)
		}
		if s.Label == "" || s.Exec == nil {
			t.Errorf("tool %q: missing label or handler", name)
		}
		if s.RequiresApproval {
			approvalGated++
			if s.PreviewKind == "" || s.RequiredScope == "" {
				t.Errorf("tool %q requires approval but has previewKind=%q requiredScope=%q", name, s.PreviewKind, s.RequiredScope)
			}
			if s.Timeout != 0 {
				t.Errorf("tool %q: timeout %s is ignored for an approval tool — its applier bounds its own phases", name, s.Timeout)
			}
		} else if s.PreviewKind != "" || s.RequiredScope != "" {
			t.Errorf("read-only tool %q must not carry previewKind/requiredScope (%q/%q)", name, s.PreviewKind, s.RequiredScope)
		}
		if s.RequiredScope != "" && !auth.ValidScope(s.RequiredScope) {
			t.Errorf("tool %q: %q is not a token scope", name, s.RequiredScope)
		}
		if s.RequiresApproval != ToolRequiresApproval(name) {
			t.Errorf("ToolRequiresApproval(%q) disagrees with the spec", name)
		}
	}
	if approvalGated != 2 {
		t.Errorf("approval-gated tools = %d, want apply_fix and apply_fixes", approvalGated)
	}
	if ToolRequiresApproval("no-such-tool") {
		t.Error("an unknown tool must not require approval")
	}
}

// TestToolSpecsGolden pins one line per tool with its flags — the frontend's ApprovalCard renders by previewKind,
// and the authz matrix assumes which scope each mutating tool needs — then every tool's description and canonical
// input schema, the text the model reads (R-testing-4). Refresh for an intended change only:
// go test ./internal/ai/ -run TestToolSpecsGolden -update-tools
func TestToolSpecsGolden(t *testing.T) {
	checkGolden(t, filepath.Join("testdata", "tools.golden"), renderToolsGolden(t), *updateTools, "-update-tools")
}

// renderToolsGolden renders the registry as tools.golden holds it.
func renderToolsGolden(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("# name | label | timeout | approval | scope | previewKind\n")
	for _, s := range ToolSpecs() {
		timeout := "default"
		if s.RequiresApproval {
			timeout = "applier"
		} else if s.Timeout > 0 {
			timeout = s.Timeout.String()
		}
		scope, kind := s.RequiredScope, s.PreviewKind
		if scope == "" {
			scope = "-"
		}
		if kind == "" {
			kind = "-"
		}
		fmt.Fprintf(&b, "%s | %s | %s | %t | %s | %s\n", s.Def.Name, s.Label, timeout, s.RequiresApproval, scope, kind)
	}
	for _, s := range ToolSpecs() {
		fmt.Fprintf(&b, "\n## %s\n%s\nschema: %s\n", s.Def.Name, s.Def.Description, canonicalJSON(t, s.Def.InputSchema))
	}
	return b.String()
}

// canonicalJSON re-encodes raw compact with sorted keys, so the golden pins content rather than formatting.
func canonicalJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("invalid JSON %s: %v", raw, err)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// checkGolden compares got with the golden at path, CR stripped from the file the way every golden here compares
// (R-testing-19: an editor or copy that re-ends the file must not produce a whole-file diff), and rewrites it when
// update is set.
func checkGolden(t *testing.T, path, got string, update bool, flagName string) {
	t.Helper()
	if update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with %s to create it)", err, flagName)
	}
	if want := strings.ReplaceAll(string(raw), "\r\n", "\n"); want != got {
		t.Errorf("%s differs; review and refresh with %s\n--- want\n%s--- got\n%s", filepath.Base(path), flagName, want, got)
	}
}

// TestToolDefinitionsFor_ReadOnlyTokenIsNotOfferedApplyFix: the tools a stream offers follow what its actor could
// execute. A chat-only token's model never sees apply_fix/apply_fixes (it would burn a paid turn on a refusal);
// chat+write sees all eight, as do an interactive session and local mode's nil actor. Without an applier wired
// the approval-gated tools are not offered either.
func TestToolDefinitionsFor_ReadOnlyTokenIsNotOfferedApplyFix(t *testing.T) {
	cases := []struct {
		name   string
		actor  *auth.Actor
		wired  bool
		want   int
		noFix  bool
		reason string
	}{
		{"chat-only token", scopedTokenActor(auth.ScopeChat), true, 6, true, ToolRefusedScope},
		{"chat+read token", scopedTokenActor(auth.ScopeChat, auth.ScopeRead), true, 6, true, ToolRefusedScope},
		{"chat+write token", scopedTokenActor(auth.ScopeChat, auth.ScopeWrite), true, 8, false, ""},
		{"interactive session", &auth.Actor{Kind: auth.ActorUser, UserID: "u-1", Role: auth.RoleMember}, true, 8, false, ""},
		{"nil actor (local mode)", nil, true, 8, false, ""},
		{"no applier wired", nil, false, 6, true, ToolRefusedApprovalUnavailable},
		{"chat-only token, no applier", scopedTokenActor(auth.ScopeChat), false, 6, true, ToolRefusedApprovalUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := NewToolGate(tc.actor, tc.wired, true)
			defs := ToolDefinitionsFor(gate)
			names := strings.Join(toolNames(defs), ",")
			if len(defs) != tc.want {
				t.Errorf("offered %d tools (%s), want %d", len(defs), names, tc.want)
			}
			hasFix := strings.Contains(names, "apply_fix")
			if hasFix == tc.noFix {
				t.Errorf("apply_fix offered=%v, want %v (%s)", hasFix, !tc.noFix, names)
			}
			spec, _ := ToolSpecOf("apply_fix")
			if got := ToolRefusal(spec, tc.actor, tc.wired, true); got != tc.reason {
				t.Errorf("ToolRefusal(apply_fix) = %q, want %q", got, tc.reason)
			}
			if got := ToolRefusal(must(ToolSpecOf("search_flow")), tc.actor, tc.wired, true); got != "" {
				t.Errorf("search_flow refused with %q", got)
			}
			// The prompt-loop instructions list the same set.
			instr := ToolPromptInstructionsFor(gate)
			if strings.Contains(instr, "- apply_fix:") == tc.noFix {
				t.Errorf("prompt instructions list apply_fix=%v, want %v", !tc.noFix, !tc.noFix)
			}
			if !strings.Contains(instr, "- search_flow:") {
				t.Error("prompt instructions must list the read-only tools")
			}
		})
	}
	if n := len(ToolDefinitionsFor(nil)); n != 8 {
		t.Errorf("a nil gate must offer every tool, got %d", n)
	}
}

func must(s ToolSpec, ok bool) ToolSpec {
	if !ok {
		panic("unknown tool")
	}
	return s
}

// TestExecuteTool_GatedToolRefusedBeforeHandler: the execution-side check is the security boundary. A call the
// gate refuses (a hallucinated or replayed apply_fix, an agent's own function_call) answers the model with a
// refusal and never reaches the handler, so no approval card is shown and nothing is applied.
func TestExecuteTool_GatedToolRefusedBeforeHandler(t *testing.T) {
	tctx := ToolContext{
		Ctx:   context.Background(),
		Doc:   toolFixtureDoc(),
		Fixes: failingFixApplier{t},
		Allow: func(s ToolSpec) string {
			if s.RequiresApproval {
				return ToolRefusedScope
			}
			return ""
		},
	}
	for _, name := range []string{"apply_fix", "apply_fixes"} {
		out := ExecuteTool(name, []byte(`{"blockId":"b1","ruleId":"r","targets":[{"blockId":"b1","ruleId":"r"}]}`), tctx)
		if !strings.HasPrefix(out, "error:") || !strings.Contains(out, name) || !strings.Contains(out, "not available in this session") {
			t.Errorf("%s: got %q, want the refusal", name, out)
		}
	}
	// The gate is consulted for every tool, and an allowed one runs.
	if out := ExecuteTool("search_flow", []byte(`{"query":"zzz-no-match"}`), tctx); strings.HasPrefix(out, "error:") {
		t.Errorf("search_flow refused by a gate that allows it: %q", out)
	}
	// An unknown tool is still "unknown", not "not available": the model should not learn a tool exists.
	if out := ExecuteTool("foo", nil, tctx); out != "error: unknown tool foo" {
		t.Errorf("unknown tool: %q", out)
	}
	// The real gate from a chat-only token refuses the same way.
	tctx.Allow = NewToolGate(scopedTokenActor(auth.ScopeChat), true, true)
	if out := ExecuteTool("apply_fix", []byte(`{"blockId":"b1","ruleId":"r"}`), tctx); !strings.Contains(out, "not available in this session") {
		t.Errorf("chat-only token: %q", out)
	}
}
