package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/auth"
	"pad-core/ai/scrubber"
	"pad-core/analyzer"
	"pad-core/models"

	"github.com/google/uuid"
)

// maxToolResultBytes caps a tool result so a single tool call can't blow the
// context budget. Results are truncated with a marker when they exceed it.
const maxToolResultBytes = 6000

// defaultToolTimeout bounds a single tool execution: the grounding tools are in-memory scans; only apply_fix
// runs longer (it waits for a human decision) and carries its own per-tool timeout.
const defaultToolTimeout = 30 * time.Second

// FixDecisionTimeout bounds how long apply_fix waits for the user's approve/decline (nothing is written on
// timeout). Exported so the chat service's applier shares the number; below the 90s stream idle timeout.
const FixDecisionTimeout = 60 * time.Second

// fixApplyToolTimeout covers the decision wait plus the apply + re-analyze
// round-trip on a large flow.
const fixApplyToolTimeout = FixDecisionTimeout + 60*time.Second

// ToolAnalysis is the slice of the analysis service the tools need, declared here so the ai package stays a
// leaf of service (no dependency cycle). *service.AnalysisService satisfies it.
type ToolAnalysis interface {
	AnalyzeFlow(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error)
	GetVariableLineage(doc *models.FlowDocument, varName string) (*models.VariableHistory, error)
}

// FixProposal describes one auto-fix awaiting approval: everything the apply path needs (fix dispatch re-runs
// server-side against the REAL, unscrubbed flow — the model never handles patch application) plus the
// human-readable Summary, rendered from the scrubbed doc.
type FixProposal struct {
	ProposalID  string
	RuleID      string
	FixType     string
	BlockID     string
	BlockLabel  string
	Line        int
	Variable    string // finding metadata the fixer dispatch needs
	Property    string
	Fingerprint string // content-stable finding identity — survives the re-parse the apply triggers
	Summary     string
}

// ToolFixApplier is the human-in-the-loop apply hook implemented by the chat service: show the proposal to
// the stream's owner, wait for their decision (bounded), on approval apply to the real flow, re-analyze and
// report. An error is returned only when the stream itself is failing.
type ToolFixApplier interface {
	ApplyFixWithApproval(ctx context.Context, prop FixProposal) (string, error)
	// ApplyFixesWithApproval requests ONE approval for a batch, then applies each sequentially
	// (re-associating targets on the re-parsed flow between applies — a patch shifts lines and mints new IDs).
	ApplyFixesWithApproval(ctx context.Context, props []FixProposal) (string, error)
}

// ToolContext is the data a tool handler operates on: Doc MUST already be scrubbed by the caller; Analysis
// may be nil; Fixes is nil unless the approval-gated apply path is wired.
type ToolContext struct {
	Ctx context.Context
	// Doc is the SCRUBBED document the model sees: every string rendered from
	// it (search results, block details, patch previews) is safe to return.
	Doc *models.FlowDocument
	// RealDoc is the unscrubbed document, SERVER-SIDE ONLY — never render from it without ScrubText.
	// Analyses and fix resolution run against it (value-dependent findings vanish on the redacted copy);
	// nil falls back to Doc.
	RealDoc  *models.FlowDocument
	Analysis ToolAnalysis
	Fixes    ToolFixApplier
	// Allow is the session's tool gate (NewToolGate): the same one that decided which tools were offered, asked
	// again by ExecuteTool before any handler runs. A model can name a tool it was not offered (a hallucinated
	// call, a replayed history, an Azure agent's own function_call), so the offer is not the boundary; this is.
	// nil allows every tool (local mode, tests).
	Allow ToolGate
}

// analysisDoc returns the document analyses run against: RealDoc when wired, else the scrubbed Doc. The block
// handed to the fixer still comes from Doc — patch previews render property VALUES, which must stay masked.
func (tctx ToolContext) analysisDoc() *models.FlowDocument {
	if tctx.RealDoc != nil {
		return tctx.RealDoc
	}
	return tctx.Doc
}

type toolHandler func(tctx ToolContext, input json.RawMessage) (string, error)

// PreviewKindFixPatch is the preview kind of the fix tools: the approval card renders each item as a patch
// preview. The frontend's ApprovalCard keys its previews on this value (ADR 0026).
const PreviewKindFixPatch = "fix-patch"

// ToolSpec is one registered tool: its wire definition plus what the loop, the gate and the approval card need
// to know about it. Def is the only part a provider sees; the converters stay untouched by the rest.
type ToolSpec struct {
	Def   ToolDefinition
	Label string // friendly status-line label, e.g. "Searching flow"
	// Timeout bounds this tool's execution (0 ⇒ defaultToolTimeout). Only the
	// approval-gated tools override it — their decision wait alone can exceed the default.
	Timeout time.Duration
	// RequiresApproval: the handler asks the stream's owner before any effect (an approval card).
	RequiresApproval bool
	// RequiredScope is the token scope an actor needs to run the tool ("" for read-only tools); a scoped API
	// token without it is not offered the tool and is refused if it calls it anyway.
	RequiredScope string
	// PreviewKind names how the approval card previews the request ("" for read-only tools).
	PreviewKind string
	Exec        toolHandler
}

// ToolGate decides whether a session may use a tool: built once per stream with NewToolGate, it selects the
// tools offered to the model (ToolDefinitionsFor) and is asked again at execution (ToolContext.Allow).
type ToolGate func(ToolSpec) bool

// The reasons ToolRefusal gives, the label values of pad_chat_tool_refused_total.
const (
	// ToolRefusedScope: a scoped API token without the tool's RequiredScope.
	ToolRefusedScope = "scope"
	// ToolRefusedApprovalUnavailable: the tool needs an approval and the session has no applier wired (ToolContext.Fixes).
	ToolRefusedApprovalUnavailable = "approval_unavailable"
)

// ToolRefusal reports why actor may not use spec in a session where approvalsWired says whether an approval
// applier exists, or "" when it may. A nil actor is local mode, which restricts nothing (the same rule as the
// fix applier's scope check); an unscoped session or token allows every scope.
func ToolRefusal(spec ToolSpec, actor *auth.Actor, approvalsWired bool) string {
	if spec.RequiresApproval && !approvalsWired {
		return ToolRefusedApprovalUnavailable
	}
	if spec.RequiredScope != "" && actor != nil && !actor.Allows(spec.RequiredScope) {
		return ToolRefusedScope
	}
	return ""
}

// NewToolGate builds the gate of one session from its actor and whether approvals can be requested.
func NewToolGate(actor *auth.Actor, approvalsWired bool) ToolGate {
	return func(spec ToolSpec) bool { return ToolRefusal(spec, actor, approvalsWired) == "" }
}

// toolRegistry is the grounding and fix tool set the model can be offered.
var toolRegistry = buildToolRegistry()

// ToolSpecs returns every registered tool, in a stable order.
func ToolSpecs() []ToolSpec {
	names := make([]string, 0, len(toolRegistry))
	for n := range toolRegistry {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ToolSpec, 0, len(names))
	for _, n := range names {
		out = append(out, toolRegistry[n])
	}
	return out
}

// ToolSpecOf returns the spec of a registered tool.
func ToolSpecOf(name string) (ToolSpec, bool) {
	t, ok := toolRegistry[name]
	return t, ok
}

// ToolDefinitionsFor returns the tool schemas to attach to a provider Request for a session gated by gate (nil
// ⇒ every tool), in a stable order.
func ToolDefinitionsFor(gate ToolGate) []ToolDefinition {
	specs := ToolSpecs()
	out := make([]ToolDefinition, 0, len(specs))
	for _, s := range specs {
		if gate == nil || gate(s) {
			out = append(out, s.Def)
		}
	}
	return out
}

// ToolDefinitions returns every tool's schema: the closed set behind the metric labels. A stream's request
// uses ToolDefinitionsFor with its gate.
func ToolDefinitions() []ToolDefinition { return ToolDefinitionsFor(nil) }

// ToolRequiresApproval reports whether a registered tool asks the user before acting (apply_fix, apply_fixes);
// false for an unknown name.
func ToolRequiresApproval(name string) bool {
	t, ok := toolRegistry[name]
	return ok && t.RequiresApproval
}

// ToolLabel returns a short human-readable status label for a tool (for the
// transient UI status line). Falls back to the tool name.
func ToolLabel(name string) string {
	if t, ok := toolRegistry[name]; ok && t.Label != "" {
		return t.Label
	}
	return name
}

// ExecuteTool runs a tool and always returns a string result — unknown tools, gated tools, bad input, handler
// errors and timeouts included — so the model can read the outcome and recover instead of the loop aborting.
func ExecuteTool(name string, input json.RawMessage, tctx ToolContext) string {
	t, ok := toolRegistry[name]
	if !ok {
		return "error: unknown tool " + name
	}
	// The session's gate, before the handler: the offer is not the boundary (see ToolContext.Allow).
	if tctx.Allow != nil && !tctx.Allow(t) {
		return fmt.Sprintf("error: tool %s is not available in this session — nothing was changed; explain the change so the user can make it instead", name)
	}
	// Malformed argument JSON (a call cut mid-argument, or the malformed JSON open models emit) becomes the
	// standard retryable error result so the model re-issues the call; empty input runs.
	if in := bytes.TrimSpace(input); len(in) > 0 && !json.Valid(in) {
		return fmt.Sprintf("error: the arguments for %s were not valid JSON — call it again with valid JSON arguments", name)
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	if tctx.Ctx != nil {
		ctx, cancel := context.WithTimeout(tctx.Ctx, timeout)
		defer cancel()
		tctx.Ctx = ctx
	}
	res, err := t.Exec(tctx, input)
	if err != nil {
		if tctx.Ctx != nil && tctx.Ctx.Err() != nil && errors.Is(tctx.Ctx.Err(), context.DeadlineExceeded) {
			return fmt.Sprintf("error: tool %s timed out after %s — simplify the request or try fewer results", name, timeout)
		}
		// Every success path scrubs; the error branch must too — a handler whose
		// error embeds flow content would otherwise bypass the scrubber on the
		// provider path.
		return "error: " + scrubber.ScrubText(err.Error())
	}
	return truncateResult(res)
}

// truncateResult caps a tool result at maxToolResultBytes without splitting a UTF-8 rune — a mid-rune slice
// would feed the model (and any log surface) invalid UTF-8.
func truncateResult(s string) string {
	if len(s) <= maxToolResultBytes {
		return s
	}
	cut := maxToolResultBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…(result truncated)"
}

func schema(raw string) json.RawMessage { return json.RawMessage(raw) }

func buildToolRegistry() map[string]ToolSpec {
	reg := map[string]ToolSpec{}
	add := func(t ToolSpec) { reg[t.Def.Name] = t }

	add(ToolSpec{
		Label: "Searching flow",
		Def: ToolDefinition{
			Name:        "search_flow",
			Description: "Search the flow for blocks whose name, action type, variables, or property values contain the query. Returns matching blocks with their IDs and subflow.",
			InputSchema: schema(`{"type":"object","properties":{"query":{"type":"string","description":"Case-insensitive substring to search for"},"limit":{"type":"integer","description":"Max results (default 15)"}},"required":["query"]}`),
		},
		Exec: toolSearchFlow,
	})
	add(ToolSpec{
		Label: "Reading block",
		Def: ToolDefinition{
			Name:        "get_block",
			Description: "Get full detail for a single block by its ID: type, action, properties, variables, line number, and child count.",
			InputSchema: schema(`{"type":"object","properties":{"blockId":{"type":"string"}},"required":["blockId"]}`),
		},
		Exec: toolGetBlock,
	})
	add(ToolSpec{
		Label: "Listing subflow blocks",
		Def: ToolDefinition{
			Name:        "get_subflow_blocks",
			Description: "List the top-level blocks of a subflow (by subflow ID) as an outline: block ID, type, name, line.",
			InputSchema: schema(`{"type":"object","properties":{"subflowId":{"type":"string"}},"required":["subflowId"]}`),
		},
		Exec: toolGetSubflowBlocks,
	})
	add(ToolSpec{
		Label: "Listing findings",
		Def: ToolDefinition{
			Name:        "list_findings",
			Description: "List analysis findings, optionally filtered by category (e.g. Security, Reliability, Performance) and/or subflow ID. Each line carries the ruleId, the finding key (usable as a finding:<key> link for the user), and the fix type when auto-fixable — use those with propose_fix/apply_fix. A line marked stage=preview comes from a preview rule: reported, but it never fails a CI gate or policy and is left out of the health score. Results are paginated: pass offset to page through everything. Requires analysis to be available.",
			InputSchema: schema(`{"type":"object","properties":{"category":{"type":"string"},"subflowId":{"type":"string"},"offset":{"type":"integer","description":"Skip the first N matching findings (pagination)"}}}`),
		},
		Exec: toolListFindings,
	})
	add(ToolSpec{
		Label: "Previewing fix",
		Def: ToolDefinition{
			Name:        "propose_fix",
			Description: "Preview the auto-fix patch for a finding WITHOUT changing anything: what would be inserted/wrapped/removed/replaced and on which lines. Input is the finding's blockId + ruleId from list_findings. Show the preview to the user; use apply_fix only when they want it applied.",
			InputSchema: schema(`{"type":"object","properties":{"blockId":{"type":"string","description":"Block ID of the finding"},"ruleId":{"type":"string","description":"Rule ID of the finding"}},"required":["blockId","ruleId"]}`),
		},
		Exec: toolProposeFix,
	})
	add(ToolSpec{
		Label: "Requesting fix approval",
		Def: ToolDefinition{
			Name:        "apply_fix",
			Description: "Apply a finding's auto-fix to the flow. ALWAYS requires explicit user approval: the user sees the patch preview and an approve/decline prompt, and nothing is written unless they approve. Call this only when the user asked to fix the issue; prefer propose_fix first when they just want to see the change.",
			InputSchema: schema(`{"type":"object","properties":{"blockId":{"type":"string","description":"Block ID of the finding"},"ruleId":{"type":"string","description":"Rule ID of the finding"}},"required":["blockId","ruleId"]}`),
		},
		Exec:             toolApplyFix,
		Timeout:          fixApplyToolTimeout,
		RequiresApproval: true,
		RequiredScope:    auth.ScopeWrite,
		PreviewKind:      PreviewKindFixPatch,
	})
	add(ToolSpec{
		Label: "Requesting batch fix approval",
		Def: ToolDefinition{
			Name:        "apply_fixes",
			Description: "Apply auto-fixes for MULTIPLE findings behind ONE approval prompt (use this instead of repeated apply_fix when the user wants several fixes, e.g. \"fix everything\"). All targets are previewed together and the user approves or declines the whole batch. Same approval rule as apply_fix: nothing is written without explicit approval.",
			InputSchema: schema(`{"type":"object","properties":{"targets":{"type":"array","minItems":1,"maxItems":10,"description":"Findings to fix, each as {blockId, ruleId} from list_findings","items":{"type":"object","properties":{"blockId":{"type":"string"},"ruleId":{"type":"string"}},"required":["blockId","ruleId"]}}},"required":["targets"]}`),
		},
		Exec:             toolApplyFixes,
		Timeout:          fixApplyToolTimeout,
		RequiresApproval: true,
		RequiredScope:    auth.ScopeWrite,
		PreviewKind:      PreviewKindFixPatch,
	})
	add(ToolSpec{
		Label: "Tracing variable",
		Def: ToolDefinition{
			Name:        "get_variable_lineage",
			Description: "Trace a variable's lifecycle across the flow: where it is initialized, mutated, and read (with block IDs and lines).",
			InputSchema: schema(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
		},
		Exec: toolGetVariableLineage,
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
	// Block/subflow NAMES are never masked by ScrubDocument (only Properties are) — a secret pasted into a
	// block name would reach the model verbatim. ScrubText as defense in depth.
	return scrubber.ScrubText(fmt.Sprintf("%d match(es) for %q:\n%s", len(matches), in.Query, strings.Join(matches, "\n"))), nil
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
	// Block/subflow names are unmasked by ScrubDocument — see toolSearchFlow.
	return scrubber.ScrubText(sb.String()), nil
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
	// Block/subflow names are unmasked by ScrubDocument — see toolSearchFlow.
	return scrubber.ScrubText(fmt.Sprintf("Subflow %s (%s), %d top-level block(s):\n%s",
		sf.ID, sf.Name, len(lines), strings.Join(lines, "\n"))), nil
}

func toolListFindings(tctx ToolContext, input json.RawMessage) (string, error) {
	var in struct {
		Category  string `json:"category"`
		SubflowID string `json:"subflowId"`
		Offset    int    `json:"offset"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
	}
	if tctx.Analysis == nil || tctx.Doc == nil {
		return "analysis not available", nil
	}
	report, err := tctx.Analysis.AnalyzeFlow(tctx.Ctx, tctx.analysisDoc())
	if err != nil || report == nil {
		return "analysis not available", nil
	}
	cat := strings.ToLower(strings.TrimSpace(in.Category))
	if in.Offset < 0 {
		in.Offset = 0
	}
	// Two passes: filter (all matches) then window — counting the full set first is what lets the model page
	// through EVERYTHING (the old hard cap of 40 made large flows unenumerable).
	var matching []string
	for _, f := range report.Findings {
		if cat != "" && strings.ToLower(f.Category) != cat {
			continue
		}
		if in.SubflowID != "" && f.SubflowID != in.SubflowID {
			continue
		}
		// Titles can embed evidence from the REAL doc's properties — ScrubText before the string leaves the
		// server. The finding key lets the model emit finding:<key> deep links.
		line := fmt.Sprintf("[%s/%s] %s: %s — block=%s subflow=%s",
			f.Severity, firstNonEmpty(f.Category, "uncategorized"), f.RuleID,
			scrubber.ScrubText(f.Title), f.BlockID, f.SubflowID)
		if f.Fingerprint != "" {
			line += fmt.Sprintf(" key=%s", f.Fingerprint)
		}
		if f.AutoFix != "" {
			line += fmt.Sprintf(" fix=%s", f.AutoFix)
		}
		if !analyzer.Gates(&f) {
			line += " stage=preview"
		}
		matching = append(matching, line)
	}
	if len(matching) == 0 {
		return "No findings match the filter.", nil
	}
	end := in.Offset + maxFindingsPerPage
	if end > len(matching) {
		end = len(matching)
	}
	if in.Offset >= len(matching) {
		return fmt.Sprintf("offset %d is beyond the %d matching finding(s).", in.Offset, len(matching)), nil
	}
	page := matching[in.Offset:end]
	out := fmt.Sprintf("%d finding(s) (showing %d-%d of %d):\n%s",
		len(page), in.Offset+1, end, len(matching), strings.Join(page, "\n"))
	if end < len(matching) {
		out += fmt.Sprintf("\n…and %d more — call again with offset=%d.", len(matching)-end, end)
	}
	return out, nil
}

// maxFindingsPerPage bounds one list_findings page. 40 keeps a page readable
// (and cheap) while the offset parameter lets the model enumerate everything.
const maxFindingsPerPage = 40

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
	return scrubber.ScrubText(fmt.Sprintf("Lineage for %q (%d event(s)):\n%s", hist.Name, len(lines), strings.Join(lines, "\n"))), nil
}

// --- fix tools ---------------------------------------------------------------

// fixTargetInput is the shared input shape of propose_fix/apply_fix: a finding
// referenced by its blockId + ruleId exactly as list_findings reports them.
type fixTargetInput struct {
	BlockID string `json:"blockId"`
	RuleID  string `json:"ruleId"`
}

// resolveFixTarget finds the finding (and its block) for a blockId+ruleId pair, returning a model-readable
// explanation when the reference doesn't resolve or isn't auto-fixable. The finding comes from REAL-doc
// analysis (value-dependent rules see the true properties); the block comes from the scrubbed Doc so the
// patch preview renders masked values — IDs are identical across the two copies.
func resolveFixTarget(tctx ToolContext, in fixTargetInput) (*models.Finding, *models.Block, string) {
	if strings.TrimSpace(in.BlockID) == "" || strings.TrimSpace(in.RuleID) == "" {
		return nil, nil, "error: blockId and ruleId are required — take both from list_findings"
	}
	if tctx.Analysis == nil || tctx.Doc == nil {
		return nil, nil, "analysis not available — ask the user to run an analysis first"
	}
	report, err := tctx.Analysis.AnalyzeFlow(tctx.Ctx, tctx.analysisDoc())
	if err != nil || report == nil {
		return nil, nil, "analysis not available"
	}
	var finding *models.Finding
	for i := range report.Findings {
		f := &report.Findings[i]
		if f.BlockID == in.BlockID && f.RuleID == in.RuleID {
			finding = f
			break
		}
	}
	if finding == nil {
		return nil, nil, fmt.Sprintf("no finding for rule %q on block %q — check list_findings for the current ruleId/blockId pair", in.RuleID, in.BlockID)
	}
	if finding.AutoFix == "" {
		return nil, nil, fmt.Sprintf("finding %q on block %s has no auto-fix — explain the issue and suggest a manual change instead", scrubber.ScrubText(finding.Title), in.BlockID)
	}
	block := tctx.Doc.BlocksByID[in.BlockID]
	if block == nil {
		return nil, nil, fmt.Sprintf("block %q not found in the flow", in.BlockID)
	}
	return finding, block, ""
}

// buildFixProposal resolves the target and computes the patch preview, shared by propose_fix (returns it)
// and apply_fix (carries it into the approval prompt). The second return is a model-readable explanation
// when the fixer declines/errs.
func buildFixProposal(tctx ToolContext, in fixTargetInput) (FixProposal, string) {
	finding, block, explain := resolveFixTarget(tctx, in)
	if explain != "" {
		return FixProposal{}, explain
	}
	// Behaviour-changing fixes are not offered through the assistant: they alter what the flow DOES at
	// runtime and the approval card carries no risk signal (no "yes, I understand" as `bakicli fix -unsafe`
	// has) — until the card can express that, the assistant explains the issue instead.
	if analyzer.IsBehaviourChangingFix(finding.AutoFix) {
		return FixProposal{}, fmt.Sprintf(
			"the %q fix changes what this flow does at runtime, so it is not applied through chat — "+
				"explain the issue and tell the user to apply it deliberately from the findings list "+
				"or with `bakicli fix -unsafe`", finding.AutoFix)
	}
	variable, _ := finding.Metadata["variable"].(string)
	property, _ := finding.Metadata["property"].(string)
	patch, err := analyzer.PatchForFix(block, finding.AutoFix, finding.RuleID, variable, property)
	if err != nil {
		return FixProposal{}, fmt.Sprintf("error: fixer for %q failed: %v", finding.AutoFix, err)
	}
	if len(patch.Ops) == 0 {
		return FixProposal{}, fmt.Sprintf("the %q fixer declined this finding (its required context is missing) — suggest a manual change instead", finding.AutoFix)
	}
	label := firstNonEmpty(block.Name, block.RawType)
	return FixProposal{
		ProposalID:  uuid.NewString(),
		RuleID:      finding.RuleID,
		FixType:     finding.AutoFix,
		BlockID:     block.ID,
		BlockLabel:  label,
		Line:        block.LineNumber,
		Variable:    variable,
		Property:    property,
		Fingerprint: finding.Fingerprint,
		Summary:     renderPatchPreview(finding, block, patch),
	}, ""
}

// renderPatchPreview produces the human/model-readable description of what a patch would change. The patch
// was computed from the SCRUBBED doc, so echoed values are masked; ScrubText is re-applied as defense in depth.
func renderPatchPreview(finding *models.Finding, block *models.Block, patch models.Patch) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Fix %q for rule %q on block %s (%s, line %d):\n",
		finding.AutoFix, finding.RuleID, block.ID, block.RawType, block.LineNumber)
	for _, op := range patch.Ops {
		switch op.Kind {
		case "insert":
			fmt.Fprintf(&sb, "  insert %d line(s) before line %d:\n", len(op.Lines), op.BeforeLine)
			for _, l := range op.Lines {
				fmt.Fprintf(&sb, "    + %s\n", trimValue(l))
			}
		case "wrap":
			delta := ""
			if op.IndentDelta > 0 {
				delta = fmt.Sprintf(" (re-indent +%d level)", op.IndentDelta)
			}
			fmt.Fprintf(&sb, "  wrap lines %d-%d%s:\n", op.StartLine, op.EndLine, delta)
			if op.Header != "" {
				for _, l := range strings.Split(op.Header, "\n") {
					fmt.Fprintf(&sb, "    + %s\n", trimValue(l))
				}
			}
			fmt.Fprintf(&sb, "    · lines %d-%d kept, re-indented\n", op.StartLine, op.EndLine)
			if op.Footer != "" {
				for _, l := range strings.Split(op.Footer, "\n") {
					fmt.Fprintf(&sb, "    + %s\n", trimValue(l))
				}
			}
		case "remove":
			fmt.Fprintf(&sb, "  remove lines %d-%d\n", op.StartLine, op.EndLine)
		case "replace":
			fmt.Fprintf(&sb, "  on line %d replace %s with %s\n", op.StartLine, trimValue(op.Old), trimValue(op.New))
		case "append":
			if len(op.Lines) > 0 {
				fmt.Fprintf(&sb, "  append to line %d: %s\n", op.StartLine, trimValue(op.Lines[0]))
			}
		default:
			fmt.Fprintf(&sb, "  %s (lines %d-%d)\n", op.Kind, op.StartLine, op.EndLine)
		}
	}
	return scrubber.ScrubText(strings.TrimSuffix(sb.String(), "\n"))
}

func toolProposeFix(tctx ToolContext, input json.RawMessage) (string, error) {
	var in fixTargetInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	prop, explain := buildFixProposal(tctx, in)
	if explain != "" {
		return explain, nil
	}
	return prop.Summary + "\n(Preview only — nothing changes until the user approves an apply_fix.)", nil
}

func toolApplyFix(tctx ToolContext, input json.RawMessage) (string, error) {
	var in fixTargetInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if tctx.Fixes == nil {
		return "error: applying fixes is not available in this session — show the user the proposed change with propose_fix instead", nil
	}
	prop, explain := buildFixProposal(tctx, in)
	if explain != "" {
		return explain, nil
	}
	return tctx.Fixes.ApplyFixWithApproval(tctx.Ctx, prop)
}

// maxBatchFixTargets bounds one apply_fixes batch: each item costs a sequential apply + re-parse +
// re-analysis; 10 stays inside fixApplyToolTimeout while covering every realistic "fix these" request.
const maxBatchFixTargets = 10

func toolApplyFixes(tctx ToolContext, input json.RawMessage) (string, error) {
	var in struct {
		Targets []fixTargetInput `json:"targets"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if len(in.Targets) == 0 {
		return "error: targets is required — pass the blockId+ruleId pairs from list_findings", nil
	}
	if len(in.Targets) > maxBatchFixTargets {
		return fmt.Sprintf("error: at most %d fixes per batch — split the request or use list_findings to pick the most important ones", maxBatchFixTargets), nil
	}
	if tctx.Fixes == nil {
		return "error: applying fixes is not available in this session — show the user the proposed changes with propose_fix instead", nil
	}
	// All-or-nothing proposal build: one unresolvable target must not leave a
	// partially-previewed batch (the user approves exactly what they saw).
	props := make([]FixProposal, 0, len(in.Targets))
	var failures []string
	for _, target := range in.Targets {
		prop, explain := buildFixProposal(tctx, target)
		if explain != "" {
			failures = append(failures, fmt.Sprintf("%s/%s: %s", target.BlockID, target.RuleID, explain))
			continue
		}
		props = append(props, prop)
	}
	if len(failures) > 0 {
		return fmt.Sprintf("error: %d of %d target(s) could not be prepared, nothing was requested — %s",
			len(failures), len(in.Targets), strings.Join(failures, "; ")), nil
	}
	return tctx.Fixes.ApplyFixesWithApproval(tctx.Ctx, props)
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
