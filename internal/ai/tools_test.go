package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"pad-core/ai/scrubber"
	"pad-core/models"
)

func toolFixtureDoc() *models.FlowDocument {
	doc := &models.FlowDocument{
		ID:   "flow-1",
		Name: "Test Flow",
		Subflows: []models.Subflow{
			{
				ID:   "sf-main",
				Name: "Main",
				Blocks: []models.Block{
					{
						ID: "b1", Name: "Connect DB", Type: models.BlockTypeAction,
						RawType: "Database.Connect", LineNumber: 3,
						Properties: map[string]string{
							"ConnectionString": "Server=db;Password=hunter2secret;",
						},
						Variables: []string{"conn"},
					},
					{
						ID: "b2", Name: "Loop rows", Type: models.BlockTypeLoop,
						RawType: "Loop", LineNumber: 5,
						Children: []models.Block{
							{ID: "b3", Name: "Write cell", Type: models.BlockTypeAction, RawType: "Excel.WriteToExcel", LineNumber: 6},
						},
					},
				},
			},
		},
	}
	doc.RebuildIndexes()
	return doc
}

// fakeAnalysis implements ToolAnalysis with canned data.
type fakeAnalysis struct {
	report *models.AnalysisReport
	hist   *models.VariableHistory
}

func (f fakeAnalysis) AnalyzeFlow(_ context.Context, _ *models.FlowDocument) (*models.AnalysisReport, error) {
	return f.report, nil
}
func (f fakeAnalysis) GetVariableLineage(_ *models.FlowDocument, _ string) (*models.VariableHistory, error) {
	return f.hist, nil
}

func TestExecuteTool_SearchFlow(t *testing.T) {
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc()}
	out := ExecuteTool("search_flow", json.RawMessage(`{"query":"loop"}`), tctx)
	if !strings.Contains(out, "b2") {
		t.Errorf("expected b2 in search results, got: %s", out)
	}
	// No match case
	out = ExecuteTool("search_flow", json.RawMessage(`{"query":"zzzznope"}`), tctx)
	if !strings.Contains(out, "No blocks matched") {
		t.Errorf("expected no-match message, got: %s", out)
	}
}

func TestExecuteTool_GetBlock_ScrubsSecrets(t *testing.T) {
	// The doc passed to tools is assumed already scrubbed by the caller, but the
	// get_block result must not leak a raw secret value. Here we feed an
	// unscrubbed fixture and assert the tool surfaces the property — the chat
	// service scrubs the doc before tools run, so we verify the value is shown
	// verbatim from the (scrubbed) doc. To prove the pipeline, scrub first.
	doc := toolFixtureDoc()
	// Simulate the chat-service scrub step.
	scrubbed, err := scrubber.ScrubDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	tctx := ToolContext{Ctx: context.Background(), Doc: scrubbed}
	out := ExecuteTool("get_block", json.RawMessage(`{"blockId":"b1"}`), tctx)
	if strings.Contains(out, "hunter2secret") {
		t.Errorf("secret leaked through get_block: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redacted ConnectionString, got: %s", out)
	}
}

func TestExecuteTool_GetBlock_NotFound(t *testing.T) {
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc()}
	out := ExecuteTool("get_block", json.RawMessage(`{"blockId":"nope"}`), tctx)
	if !strings.Contains(out, "not found") {
		t.Errorf("expected not-found, got: %s", out)
	}
}

func TestExecuteTool_GetSubflowBlocks(t *testing.T) {
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc()}
	out := ExecuteTool("get_subflow_blocks", json.RawMessage(`{"subflowId":"sf-main"}`), tctx)
	if !strings.Contains(out, "b1") || !strings.Contains(out, "b2") {
		t.Errorf("expected top-level blocks listed, got: %s", out)
	}
	if !strings.Contains(out, "+1 nested") {
		t.Errorf("expected nested-child marker for the loop, got: %s", out)
	}
}

func TestExecuteTool_ListFindings(t *testing.T) {
	report := &models.AnalysisReport{Findings: []models.Finding{
		{ID: "f1", Severity: models.SeverityError, Category: "Security", Title: "Hardcoded secret", BlockID: "b1", SubflowID: "sf-main"},
		{ID: "f2", Severity: models.SeverityWarning, Category: "Performance", Title: "Slow loop", BlockID: "b2", SubflowID: "sf-main"},
	}}
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc(), Analysis: fakeAnalysis{report: report}}

	out := ExecuteTool("list_findings", json.RawMessage(`{"category":"Security"}`), tctx)
	if !strings.Contains(out, "Hardcoded secret") || strings.Contains(out, "Slow loop") {
		t.Errorf("category filter failed, got: %s", out)
	}

	// No analysis available
	out = ExecuteTool("list_findings", json.RawMessage(`{}`), ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc()})
	if !strings.Contains(out, "analysis not available") {
		t.Errorf("expected analysis-not-available, got: %s", out)
	}
}

func TestExecuteTool_VariableLineage(t *testing.T) {
	hist := &models.VariableHistory{Name: "conn", Events: []models.VariableEvent{
		{Type: "init", BlockID: "b1", SubflowID: "sf-main", Line: 3},
	}}
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc(), Analysis: fakeAnalysis{hist: hist}}
	out := ExecuteTool("get_variable_lineage", json.RawMessage(`{"name":"conn"}`), tctx)
	if !strings.Contains(out, "init") || !strings.Contains(out, "b1") {
		t.Errorf("expected lineage event, got: %s", out)
	}
}

func TestExecuteTool_UnknownAndBadInput(t *testing.T) {
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc()}

	if got := ExecuteTool("no_such_tool", nil, tctx); !strings.Contains(got, "unknown tool") {
		t.Errorf("unknown tool: want 'unknown tool' in result, got %q", got)
	}
	if got := ExecuteTool("get_block", json.RawMessage("not json"), tctx); !strings.Contains(got, "error:") {
		t.Errorf("bad input: want 'error:' in result, got %q", got)
	}
}

// TestTruncateResult_RuneSafe proves the byte cap backs off to a UTF-8 rune
// boundary: a naive s[:6000] slice can split a multi-byte rune and feed the
// model invalid UTF-8.
func TestTruncateResult_RuneSafe(t *testing.T) {
	// 3-byte runes (☃): any byte offset ≡ 0 mod 3 except multiples past the
	// cap is mid-rune. 2001 snowmen = 6003 bytes > 6000 cap.
	in := strings.Repeat("☃", 2001)
	got := truncateResult(in)
	if !strings.HasSuffix(got, "\n…(result truncated)") {
		t.Error("want truncation marker suffix")
	}
	body := strings.TrimSuffix(got, "\n…(result truncated)")
	if n := len(body); n > maxToolResultBytes {
		t.Errorf("truncated body is %d bytes, exceeds cap %d", n, maxToolResultBytes)
	}
	if !utf8.ValidString(body) {
		t.Errorf("truncated body is not valid UTF-8 (split rune): %q", body[len(body)-8:])
	}
	if r := len([]rune(body)); r != maxToolResultBytes/3 {
		t.Errorf("want %d whole runes (2000), got %d", maxToolResultBytes/3, r)
	}
}

// TestTruncateResult_AsciiUnchanged guards the no-op path.
func TestTruncateResult_AsciiUnchanged(t *testing.T) {
	in := strings.Repeat("a", maxToolResultBytes)
	if got := truncateResult(in); got != in {
		t.Error("input at the cap must round-trip unchanged")
	}
}

func TestToolDefinitions_StableAndComplete(t *testing.T) {
	defs := ToolDefinitions()
	if len(defs) != 8 {
		t.Fatalf("expected 8 tools, got %d", len(defs))
	}
	for _, d := range defs {
		if d.Name == "" || len(d.InputSchema) == 0 {
			t.Errorf("tool %q missing name or schema", d.Name)
		}
		var js map[string]any
		if err := json.Unmarshal(d.InputSchema, &js); err != nil {
			t.Errorf("tool %q has invalid JSON schema: %v", d.Name, err)
		}
	}
}
