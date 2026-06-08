package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"pad-analyzer/internal/ai/scrubber"
	"pad-analyzer/internal/models"
)

// maxToolResultBytes caps a tool result so a single tool call can't blow the
// context budget. Results are truncated with a marker when they exceed it.
const maxToolResultBytes = 6000

// ToolAnalysis is the slice of the analysis service the tools need. Declared
// here (rather than importing internal/service) so the ai package stays a leaf
// of service, not a dependency cycle. *service.AnalysisService satisfies it.
type ToolAnalysis interface {
	AnalyzeFlow(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error)
	GetVariableLineage(doc *models.FlowDocument, varName string) (*models.VariableHistory, error)
}

// ToolContext is the data a tool handler operates on. Doc MUST already be
// scrubbed by the caller; Analysis may be nil (tools that need it say so).
type ToolContext struct {
	Ctx      context.Context
	Doc      *models.FlowDocument
	Analysis ToolAnalysis
}

type toolHandler func(tctx ToolContext, input json.RawMessage) (string, error)

type registeredTool struct {
	def     ToolDefinition
	label   string // friendly status-line label, e.g. "Searching flow"
	handler toolHandler
}

// toolRegistry is the read-only grounding tool set offered to the model.
var toolRegistry = buildToolRegistry()

// ToolDefinitions returns the tool schemas to attach to a provider Request, in a
// stable order.
func ToolDefinitions() []ToolDefinition {
	names := make([]string, 0, len(toolRegistry))
	for n := range toolRegistry {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ToolDefinition, 0, len(names))
	for _, n := range names {
		out = append(out, toolRegistry[n].def)
	}
	return out
}

// ToolLabel returns a short human-readable status label for a tool (for the
// transient UI status line). Falls back to the tool name.
func ToolLabel(name string) string {
	if t, ok := toolRegistry[name]; ok && t.label != "" {
		return t.label
	}
	return name
}

// ExecuteTool runs a tool and always returns a string result — including for
// unknown tools, bad input, or handler errors — so the model can read the
// outcome and recover rather than the loop aborting.
func ExecuteTool(name string, input json.RawMessage, tctx ToolContext) string {
	t, ok := toolRegistry[name]
	if !ok {
		return "error: unknown tool " + name
	}
	res, err := t.handler(tctx, input)
	if err != nil {
		return "error: " + err.Error()
	}
	return truncateResult(res)
}

func truncateResult(s string) string {
	if len(s) <= maxToolResultBytes {
		return s
	}
	return s[:maxToolResultBytes] + "\n…(result truncated)"
}

func schema(raw string) json.RawMessage { return json.RawMessage(raw) }

func buildToolRegistry() map[string]registeredTool {
	reg := map[string]registeredTool{}
	add := func(t registeredTool) { reg[t.def.Name] = t }

	add(registeredTool{
		label: "Searching flow",
		def: ToolDefinition{
			Name:        "search_flow",
			Description: "Search the flow for blocks whose name, action type, variables, or property values contain the query. Returns matching blocks with their IDs and subflow.",
			InputSchema: schema(`{"type":"object","properties":{"query":{"type":"string","description":"Case-insensitive substring to search for"},"limit":{"type":"integer","description":"Max results (default 15)"}},"required":["query"]}`),
		},
		handler: toolSearchFlow,
	})
	add(registeredTool{
		label: "Reading block",
		def: ToolDefinition{
			Name:        "get_block",
			Description: "Get full detail for a single block by its ID: type, action, properties, variables, line number, and child count.",
			InputSchema: schema(`{"type":"object","properties":{"blockId":{"type":"string"}},"required":["blockId"]}`),
		},
		handler: toolGetBlock,
	})
	add(registeredTool{
		label: "Listing subflow blocks",
		def: ToolDefinition{
			Name:        "get_subflow_blocks",
			Description: "List the top-level blocks of a subflow (by subflow ID) as an outline: block ID, type, name, line.",
			InputSchema: schema(`{"type":"object","properties":{"subflowId":{"type":"string"}},"required":["subflowId"]}`),
		},
		handler: toolGetSubflowBlocks,
	})
	add(registeredTool{
		label: "Listing findings",
		def: ToolDefinition{
			Name:        "list_findings",
			Description: "List analysis findings, optionally filtered by category (e.g. Security, Reliability, Performance) and/or subflow ID. Requires analysis to be available.",
			InputSchema: schema(`{"type":"object","properties":{"category":{"type":"string"},"subflowId":{"type":"string"}}}`),
		},
		handler: toolListFindings,
	})
	add(registeredTool{
		label: "Tracing variable",
		def: ToolDefinition{
			Name:        "get_variable_lineage",
			Description: "Trace a variable's lifecycle across the flow: where it is initialized, mutated, and read (with block IDs and lines).",
			InputSchema: schema(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
		},
		handler: toolGetVariableLineage,
	})
	return reg
}

// --- handlers ---------------------------------------------------------------

func toolSearchFlow(tctx ToolContext, input json.RawMessage) (string, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(in.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	if tctx.Doc == nil {
		return "no flow loaded", nil
	}
	limit := in.Limit
	if limit <= 0 || limit > 50 {
		limit = 15
	}
	q := strings.ToLower(in.Query)

	// Iterate blocks in a stable order (by ID) for deterministic output.
	ids := make([]string, 0, len(tctx.Doc.BlocksByID))
	for id := range tctx.Doc.BlocksByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var matches []string
	for _, id := range ids {
		b := tctx.Doc.BlocksByID[id]
		if b == nil || !blockMatches(b, q) {
			continue
		}
		sfName := subflowName(tctx.Doc, id)
		matches = append(matches, fmt.Sprintf("%s | %s | %s | subflow=%s (line %d)",
			b.ID, b.Type, firstNonEmpty(b.Name, b.RawType), sfName, b.LineNumber))
		if len(matches) >= limit {
			break
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("No blocks matched %q.", in.Query), nil
	}
	return fmt.Sprintf("%d match(es) for %q:\n%s", len(matches), in.Query, strings.Join(matches, "\n")), nil
}

func blockMatches(b *models.Block, q string) bool {
	if strings.Contains(strings.ToLower(b.Name), q) ||
		strings.Contains(strings.ToLower(b.RawType), q) ||
		strings.Contains(strings.ToLower(string(b.Type)), q) {
		return true
	}
	for _, v := range b.Variables {
		if strings.Contains(strings.ToLower(v), q) {
			return true
		}
	}
	for k, v := range b.Properties {
		if strings.Contains(strings.ToLower(k), q) || strings.Contains(strings.ToLower(v), q) {
			return true
		}
	}
	return false
}

func toolGetBlock(tctx ToolContext, input json.RawMessage) (string, error) {
	var in struct {
		BlockID string `json:"blockId"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if tctx.Doc == nil {
		return "no flow loaded", nil
	}
	b := tctx.Doc.BlocksByID[in.BlockID]
	if b == nil {
		return fmt.Sprintf("block %q not found", in.BlockID), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Block %s\n  type: %s\n  action: %s\n  name: %s\n  subflow: %s\n  line: %d\n  children: %d\n",
		b.ID, b.Type, b.RawType, b.Name, subflowName(tctx.Doc, b.ID), b.LineNumber, len(b.Children))
	if len(b.Variables) > 0 {
		fmt.Fprintf(&sb, "  variables: %s\n", strings.Join(b.Variables, ", "))
	}
	if len(b.Properties) > 0 {
		keys := make([]string, 0, len(b.Properties))
		for k := range b.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteString("  properties:\n")
		for _, k := range keys {
			fmt.Fprintf(&sb, "    %s: %s\n", k, trimValue(b.Properties[k]))
		}
	}
	return sb.String(), nil
}

func toolGetSubflowBlocks(tctx ToolContext, input json.RawMessage) (string, error) {
	var in struct {
		SubflowID string `json:"subflowId"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if tctx.Doc == nil {
		return "no flow loaded", nil
	}
	sf := tctx.Doc.SubflowsByID[in.SubflowID]
	if sf == nil {
		return fmt.Sprintf("subflow %q not found", in.SubflowID), nil
	}
	var lines []string
	for i := range sf.Blocks {
		b := &sf.Blocks[i]
		suffix := ""
		if len(b.Children) > 0 {
			suffix = fmt.Sprintf(" (+%d nested)", len(b.Children))
		}
		lines = append(lines, fmt.Sprintf("%s | %s | %s | line %d%s",
			b.ID, b.Type, firstNonEmpty(b.Name, b.RawType), b.LineNumber, suffix))
	}
	if len(lines) == 0 {
		return fmt.Sprintf("subflow %q (%s) has no blocks", sf.ID, sf.Name), nil
	}
	return fmt.Sprintf("Subflow %s (%s), %d top-level block(s):\n%s",
		sf.ID, sf.Name, len(lines), strings.Join(lines, "\n")), nil
}

func toolListFindings(tctx ToolContext, input json.RawMessage) (string, error) {
	var in struct {
		Category  string `json:"category"`
		SubflowID string `json:"subflowId"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
	}
	if tctx.Analysis == nil || tctx.Doc == nil {
		return "analysis not available", nil
	}
	report, err := tctx.Analysis.AnalyzeFlow(tctx.Ctx, tctx.Doc)
	if err != nil || report == nil {
		return "analysis not available", nil
	}
	cat := strings.ToLower(strings.TrimSpace(in.Category))
	var lines []string
	for _, f := range report.Findings {
		if cat != "" && strings.ToLower(f.Category) != cat {
			continue
		}
		if in.SubflowID != "" && f.SubflowID != in.SubflowID {
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s/%s] %s — block=%s subflow=%s",
			f.Severity, firstNonEmpty(f.Category, "uncategorized"),
			scrubber.ScrubText(f.Title), f.BlockID, f.SubflowID))
		if len(lines) >= 40 {
			break
		}
	}
	if len(lines) == 0 {
		return "No findings match the filter.", nil
	}
	return fmt.Sprintf("%d finding(s):\n%s", len(lines), strings.Join(lines, "\n")), nil
}

func toolGetVariableLineage(tctx ToolContext, input json.RawMessage) (string, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(in.Name) == "" {
		return "", fmt.Errorf("name is required")
	}
	if tctx.Analysis == nil || tctx.Doc == nil {
		return "analysis not available", nil
	}
	hist, err := tctx.Analysis.GetVariableLineage(tctx.Doc, in.Name)
	if err != nil || hist == nil || len(hist.Events) == 0 {
		return fmt.Sprintf("No lineage found for variable %q.", in.Name), nil
	}
	var lines []string
	for _, e := range hist.Events {
		lines = append(lines, fmt.Sprintf("%s @ block=%s subflow=%s line=%d", e.Type, e.BlockID, e.SubflowID, e.Line))
	}
	return fmt.Sprintf("Lineage for %q (%d event(s)):\n%s", hist.Name, len(lines), strings.Join(lines, "\n")), nil
}

// --- helpers ----------------------------------------------------------------

func subflowName(doc *models.FlowDocument, blockID string) string {
	if doc == nil || doc.BlockSubflow == nil {
		return ""
	}
	if sf := doc.BlockSubflow[blockID]; sf != nil {
		return sf.Name
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func trimValue(v string) string {
	const max = 200
	v = strings.ReplaceAll(v, "\n", " ")
	if len(v) > max {
		return v[:max] + "…"
	}
	return v
}
