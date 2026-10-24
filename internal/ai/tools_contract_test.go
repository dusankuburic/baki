package ai

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R-testing-4: no golden pinned the prompt-facing text — tool descriptions, input schemas, the system prompt and
// the marker instructions — so a rewording that changes model behaviour shipped without a reviewable diff. The
// tools golden now carries each description and canonical schema; prompts.golden pins the prompts. R-testing-19:
// the tools golden compares with CR stripped.

var updatePrompts = flag.Bool("update-prompts", false, "rewrite testdata/prompts.golden")

// TestPromptsGolden pins the system prompt for each tool access and the marker-mode instructions for the full and
// chat-only tool sets. Refresh for an intended change only:
// go test ./internal/ai/ -run TestPromptsGolden -update-prompts
func TestPromptsGolden(t *testing.T) {
	var b strings.Builder
	section := func(title, body string) {
		b.WriteString("=== " + title + " ===\n" + body + "\n\n")
	}
	section("SystemPromptFor(ToolAccessFixes) = SystemPromptDefault", SystemPromptFor(ToolAccessFixes))
	section("SystemPromptFor(ToolAccessReadOnly)", SystemPromptFor(ToolAccessReadOnly))
	section("SystemPromptFor(ToolAccessNone)", SystemPromptFor(ToolAccessNone))
	section("ToolPromptInstructionsFor(nil)", ToolPromptInstructionsFor(nil))
	section("ToolPromptInstructionsFor(chat-only token)", ToolPromptInstructionsFor(NewToolGate(scopedTokenActor("chat"), true, true)))
	if SystemPromptFor(ToolAccessFixes) != SystemPromptDefault {
		t.Error("SystemPromptDefault is not the fix-tools prompt the golden labels it as")
	}
	checkGolden(t, filepath.Join("testdata", "prompts.golden"), b.String(), *updatePrompts, "-update-prompts")
}

// TestToolSpecsGolden_ToleratesCRLF: a golden re-ended CRLF by an editor or a copy still compares equal.
func TestToolSpecsGolden_ToleratesCRLF(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "tools.golden"))
	if err != nil {
		t.Fatal(err)
	}
	crlf := filepath.Join(t.TempDir(), "tools.golden")
	lf := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if err := os.WriteFile(crlf, []byte(strings.ReplaceAll(lf, "\n", "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, crlf, renderToolsGolden(t), false, "-update-tools")
}

// TestApplyFixesMaxItemsIsTheBatchLimit: the schema the model reads and the limit the handler enforces agree.
func TestApplyFixesMaxItemsIsTheBatchLimit(t *testing.T) {
	var js struct {
		Properties struct {
			Targets struct {
				MaxItems int `json:"maxItems"`
			} `json:"targets"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(must(ToolSpecOf("apply_fixes")).Def.InputSchema, &js); err != nil {
		t.Fatal(err)
	}
	if js.Properties.Targets.MaxItems != maxBatchFixTargets {
		t.Errorf("apply_fixes schema maxItems = %d, handler limit maxBatchFixTargets = %d", js.Properties.Targets.MaxItems, maxBatchFixTargets)
	}
}

// jsonSchema is the slice of JSON Schema the tool definitions use.
type jsonSchema struct {
	Type       string                `json:"type"`
	Properties map[string]jsonSchema `json:"properties"`
	Required   []string              `json:"required"`
	Items      *jsonSchema           `json:"items"`
}

// sampleOf builds a value satisfying s with every required property set.
func sampleOf(s jsonSchema) any {
	switch s.Type {
	case "object":
		obj := map[string]any{}
		for _, name := range s.Required {
			obj[name] = sampleOf(s.Properties[name])
		}
		return obj
	case "array":
		if s.Items == nil {
			return []any{}
		}
		return []any{sampleOf(*s.Items)}
	case "integer", "number":
		return 1
	case "boolean":
		return true
	default:
		return "x"
	}
}

// missingFieldError reports whether a tool result is the handler's "field is required" refusal.
func missingFieldError(out string) bool {
	return strings.Contains(out, " is required") || strings.Contains(out, " are required")
}

// TestToolSchemaRequiredMatchesTheHandler: for every tool, an input with each schema-required property set gets
// past the handler's missing-field checks, and dropping any one of them is refused as missing — the schema's
// required set is the handler's.
func TestToolSchemaRequiredMatchesTheHandler(t *testing.T) {
	for _, spec := range ToolSpecs() {
		name := spec.Def.Name
		var s jsonSchema
		if err := json.Unmarshal(spec.Def.InputSchema, &s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		tctx := func() ToolContext {
			return ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc(), Analysis: fixFakeAnalysis{fixReport("b1")},
				Fixes: &stubFixApplier{ret: "APPLIED"}}
		}
		full := sampleOf(s).(map[string]any)
		in, _ := json.Marshal(full)
		if out := ExecuteTool(name, in, tctx()); missingFieldError(out) {
			t.Errorf("%s: every required field set (%s), but the handler refused one as missing: %q", name, in, out)
		}
		for _, field := range s.Required {
			partial := map[string]any{}
			for k, v := range full {
				if k != field {
					partial[k] = v
				}
			}
			in, _ := json.Marshal(partial)
			if out := ExecuteTool(name, in, tctx()); !missingFieldError(out) || !strings.Contains(out, field) {
				t.Errorf("%s: schema requires %q, but the handler accepted an input without it: %q", name, field, out)
			}
		}
	}
}
