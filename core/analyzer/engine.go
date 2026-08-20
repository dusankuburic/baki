package analyzer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"pad-core/logger"
	"pad-core/models"
)

var variableRefRegex = regexp.MustCompile(`%([^%]+)%`)

// subjectMetaKeys are the metadata keys that name WHAT a finding is about. They
// disambiguate two findings of the same rule on the same block (e.g. two
// uninitialized variables) so each gets a distinct content key. dedup.go's
// subjectKeys aliases this slice rather than redeclaring it.
var subjectMetaKeys = []string{"variable", "property", "resource"}

// ruleConfidence is a per-rule default certainty for findings. Rules not listed
// default to Medium. A rule may still set Confidence explicitly on a finding to
// override (e.g. hardcoded-credential raises to High on a regex match and
// leaves Medium on an entropy-only guess). Drives severity×confidence triage
// ordering and the UI's "maybe" affordance.
var ruleConfidence = map[string]models.Confidence{
	// High — deterministic, low false-positive rate.
	"hardcoded-credential": models.ConfidenceHigh,
	"sensitive-exposure":   models.ConfidenceHigh,
	"sql-injection-risk":   models.ConfidenceHigh,
	"resource-leak":        models.ConfidenceHigh,
	"infinite-loop-risk":   models.ConfidenceHigh,
	"empty-handler":        models.ConfidenceHigh,
	"empty-branch":         models.ConfidenceHigh,
	"redundant-action":     models.ConfidenceHigh,
	// Low — heuristic / style, frequent false positives.
	"uninitialized-variable":      models.ConfidenceLow,
	"unused-variable":             models.ConfidenceLow,
	"deep-nesting":                models.ConfidenceLow,
	"wide-loop":                   models.ConfidenceLow,
	"large-subflow":               models.ConfidenceLow,
	"hardcoded-url":               models.ConfidenceLow,
	"hardcoded-filepath":          models.ConfidenceLow,
	"hardcoded-ip":                models.ConfidenceLow,
	"circular-subflow-dependency": models.ConfidenceHigh,
	"tainted-sink":                models.ConfidenceMedium,
	"parse-error":                 models.ConfidenceHigh,
}

// ruleAutoFix maps a rule to the deterministic fix the user can apply in one
// click from the findings UI (desktop: edits the source, re-parses, re-
// analyzes). Findings for rules not listed carry no AutoFix (only prose/AI).
// Keep the fixType values in sync with FlowService.ApplyFix.
var ruleAutoFix = map[string]string{
	"unhandled-error":          "wrap-error-handler",
	"file-op-no-error-handler": "wrap-error-handler",
	"subflow-no-error-handler": "wrap-error-handler",
	"resource-leak":            "insert-close",
	"missing-timeout":          "set-timeout",
	"missing-delay":            "insert-delay",
	"empty-handler":            "insert-handler-log",
	"uninitialized-variable":   "init-variable",
	"error-swallow":            "insert-error-log",
	"missing-retry":            "wrap-in-retry",
	"infinite-loop-risk":       "insert-exit-condition",
	// remove-block: delete the offending block outright.
	"duplicate-action": "remove-block",
	"redundant-action": "remove-block",
	"dead-code":        "remove-block",
	"dead-data":        "remove-block",
	"empty-branch":     "remove-block",
	"empty-subflow":    "remove-block",
	"uncalled-subflow": "remove-block",
	"unused-variable":  "remove-block",
	"disabled-block":   "remove-block",
	"wait-zero":        "remove-block",
	"todo-in-comment":  "remove-block",
	// replace-with-variable: swap a hardcoded literal for %input_<key>%.
	"hardcoded-credential": "replace-with-variable",
	"hardcoded-filepath":   "replace-with-variable",
	"hardcoded-url":        "replace-with-variable",
	"hardcoded-ip":         "replace-with-variable",
	"sensitive-exposure":   "mask-sensitive-variable",
	"switch-no-default":    "insert-default",
	// insert-delay-in-loop: insert WAIT inside a tight loop body.
	"slow-pattern":       "insert-delay-in-loop",
	"sql-injection-risk": "parameterize-sql",
	// upgrade-to-https: replace cleartext http:// with https://.
	"insecure-http-url": "upgrade-to-https",
	// replace-with-variable: parameterize a hardcoded literal as %input_<key>%.
	"hardcoded-ui-coordinates": "replace-with-variable",
	// sanitize-command-vars: strip %VarName% from system-command properties.
	"command-injection-risk": "sanitize-command-vars",
}

// RuleConfidence returns the built-in default certainty for a rule's findings
// (High/Medium/Low), defaulting to Medium for rules not explicitly listed in
// ruleConfidence. Exposed so the rule catalog and dashboards can surface rule
// certainty without running an analysis.
func RuleConfidence(id string) models.Confidence {
	if c, ok := ruleConfidence[id]; ok {
		return c
	}
	return models.ConfidenceMedium
}

// RuleAutoFix returns the deterministic fixType a user can apply in one click
// for the given rule (e.g. "set-timeout"), or "" when the rule has no auto-
// fixer. Exposed so the catalog can show fix-availability and dashboards can
// compute "auto-fixable rule" counts. Keep the returned values in sync with
// FlowService.ApplyFix.
func RuleAutoFix(id string) string {
	return ruleAutoFix[id]
}

// directAutoFixRuleIDs lists rules that stamp a finding's AutoFix directly in
// their Check instead of relying on the engine's ruleAutoFix lookup (see
// runAnalysisCore's "if findings[i].AutoFix == ”" fill). Keep in sync with
// rules that construct findings with a non-empty AutoFix field — a rule listed
// here but stamping no AutoFix merely runs needlessly in the fix loop, but a
// rule stamping AutoFix and NOT listed here would never be fixed by the loop.
var directAutoFixRuleIDs = map[string]bool{
	"subflow-mismatch": true, // "append-output" set in Check
}

// mayEmitAutoFix reports whether a rule can produce a finding with a non-empty
// AutoFix: either via the engine's ruleAutoFix map, by setting it directly
// (directAutoFixRuleIDs), or — for custom rules — via their validated config.
func mayEmitAutoFix(r Rule) bool {
	if ruleAutoFix[r.ID()] != "" || directAutoFixRuleIDs[r.ID()] {
		return true
	}
	if cr, ok := r.(*CustomRule); ok {
		return cr.config.AutoFix != ""
	}
	return false
}

// findingContentKey derives a stable identity for a finding from the block's
// CONTENT (subflow name, rawType, name, line number) plus the rule and the
// subject — independent of the parser-minted BlockID/SubflowID UUIDs, which
// change on every re-parse of the same source. This is what Fingerprint is set
// to, so triage/baseline/diff/SARIF keys survive re-imports and CLI re-runs
// (the old RuleID:BlockID key rotted every re-parse). Falls back to the legacy
// Key() when block context is unavailable.
func findingContentKey(f models.Finding, flow *models.FlowDocument) string {
	if flow == nil {
		return f.Key()
	}
	block := flow.BlocksByID[f.BlockID]
	if block == nil {
		return f.Key()
	}
	subflowName := ""
	if sf := flow.BlockSubflow[f.BlockID]; sf != nil {
		subflowName = sf.Name
	}
	subject := ""
	for _, k := range subjectMetaKeys {
		if v, ok := f.Metadata[k].(string); ok && v != "" {
			subject = v
			break
		}
	}
	payload := strings.Join([]string{subflowName, block.RawType, block.Name, strconv.Itoa(block.LineNumber), subject}, "|")
	h := sha256.Sum256([]byte(payload))
	return f.RuleID + ":" + hex.EncodeToString(h[:8]) // 8 bytes = 16 hex chars; ~1e19 space
}

type RuleContext struct {
	Flow            *models.FlowDocument
	AllBlocks       map[string]*models.Block
	BlocksByName    map[string][]*models.Block
	BlocksByType    map[models.BlockType][]*models.Block
	Settings        *models.AppSettings
	ParentMap       map[string]string
	SiblingMap      map[string][]*models.Block
	SiblingKey      map[string]string
	UsedVariables   map[string]bool
	BlockIndex      map[string]int
	TerminatorIndex map[string]int
	BlockDepth      map[string]int
	// WritersByVar maps a variable name to the IDs of blocks that write it
	// (via the _output or _var property). ReadersByVar maps a variable name to
	// the IDs of blocks that read it (variable appears in block.Variables).
	// Both are built once during collectBlocks so data-flow lookups are O(1)
	// instead of a full AllBlocks scan per variable (was O(blocks²·vars)).
	WritersByVar map[string][]string
	ReadersByVar map[string][]string
	// FirstReaderByVar maps a variable name to the ID of its earliest-reading
	// block (lowest LineNumber). Precomputed once from ReadersByVar so the
	// uninitialized-variable rule's "is this the first usage?" check is O(1)
	// per block instead of rescanning every reader — that rescan made the rule
	// O(readers²) per variable and dominated analysis time on large flows.
	FirstReaderByVar map[string]string
	// LabelByName maps a lowercased label name to its (document-order first)
	// LABEL block. Precomputed so the goto-antipattern rule resolves a jump
	// target in O(1) instead of scanning every block per GOTO (was O(gotos·blocks)).
	LabelByName map[string]*models.Block
	// LabelNameCount counts LABEL blocks per lowercased name, filled in the
	// same pass as LabelByName. Precomputed so duplicate-label answers "how
	// many share this name?" in O(1) instead of rescanning AllBlocks per
	// LABEL block (which made the rule O(labels·blocks) + one ToLower
	// allocation per scanned block).
	LabelNameCount map[string]int
	// ClosedResourceVars maps a resource "close" action prefix to the set of
	// variables referenced by any block performing that close. Precomputed so the
	// resource-leak rule checks "is this handle closed anywhere?" in O(1) instead
	// of scanning every block per open action (was O(opens·blocks)).
	ClosedResourceVars map[string]map[string]bool
	// SubflowByID / SubflowByName resolve a subflow in O(1). Precomputed so the
	// subflow rules (large-subflow, subflow-no-error-handler resolve by ID;
	// subflow-mismatch resolves a CALL target by name) don't rescan every subflow
	// per invocation — that scan made those rules O(subflows²) / O(calls·subflows).
	// On duplicate names the first subflow in document order wins, matching the
	// previous inline scans which kept the first match and broke.
	SubflowByID   map[string]*models.Subflow
	SubflowByName map[string]*models.Subflow
	// sigCache memoizes block content signatures (see blockSig) so the
	// duplicate-action rule hashes each block at most once instead of O(run²)
	// times. Safe as a plain map: the analysis walk is single-threaded.
	sigCache    map[string]string
	totalBlocks int
	// HasErrorHandler marks every block that has (or whose ancestor chain
	// contains) an error-handler block among its siblings. Precomputed once
	// in buildContext so 3 rules (file-op-no-error-handler, missing-retry,
	// unhandled-error) do an O(1) lookup instead of each re-walking the chain
	// and allocating a visited map per call.
	HasErrorHandler map[string]bool
	// InsideRetryLoop marks every block enclosed by a LOOP whose name or
	// properties include "retry"/"attempt". Precomputed once so the
	// missing-retry rule does an O(1) lookup instead of re-walking + allocating.
	InsideRetryLoop map[string]bool
	// CircularSubflows marks every subflow ID that participates in a circular
	// dependency cycle (detected via DFS on the call graph). Precomputed once
	// so the circular-subflow-dependency rule does an O(1) lookup instead of
	// rebuilding the call graph per subflow.
	CircularSubflows map[string]bool
	// SubflowCyclo maps a subflow ID to its cyclomatic complexity. Precomputed
	// once so the high-cyclomatic-complexity rule does an O(1) lookup.
	SubflowCyclo map[string]int
	// SubflowFanIn maps a subflow ID to its fan-in count (number of distinct
	// callers). Precomputed once so the uncalled-subflow rule does an O(1)
	// lookup.
	SubflowFanIn map[string]int
	// DuplicateSubflowNames is the set of subflow names that appear more than
	// once in the flow. Precomputed once so the duplicate-subflow-name rule
	// does an O(1) lookup per subflow.
	DuplicateSubflowNames map[string]bool

	// Graph/metrics artifacts kept from buildContext so the report's metrics
	// phase reuses them instead of rebuilding the call graph, its fan-in
	// inversion, the cycle DFS, and the per-subflow complexity walk a second
	// time (ComputeFlowMetrics used to redo all four).
	callGraph      map[string]map[string]bool
	fanInByID      map[string]int
	circularDeps   []string
	subflowMetrics []models.SubflowMetrics
}

// blockSig returns the memoized content signature for b, computing it once.
func (ctx *RuleContext) blockSig(b *models.Block) string {
	if s, ok := ctx.sigCache[b.ID]; ok {
		return s
	}
	s := blockSignature(b)
	ctx.sigCache[b.ID] = s
	return s
}

type Rule interface {
	ID() string
	Name() string
	Description() string
	DefaultSeverity() models.Severity
	Category() string
	Check(block *models.Block, ctx *RuleContext) []models.Finding
}

func buildContext(flow *models.FlowDocument, settings *models.AppSettings) *RuleContext {
	ctx := &RuleContext{
		Flow:            flow,
		AllBlocks:       make(map[string]*models.Block),
		BlocksByName:    make(map[string][]*models.Block),
		BlocksByType:    make(map[models.BlockType][]*models.Block),
		ParentMap:       make(map[string]string),
		SiblingMap:      make(map[string][]*models.Block),
		SiblingKey:      make(map[string]string),
		UsedVariables:   make(map[string]bool),
		BlockIndex:      make(map[string]int),
		TerminatorIndex: make(map[string]int),
		BlockDepth:      make(map[string]int),
		WritersByVar:    make(map[string][]string),
		ReadersByVar:    make(map[string][]string),
		sigCache:        make(map[string]string),
		Settings:        settings,
	}

	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		collectBlocks(ctx, sf.Blocks, "", sf.ID, 0)
	}

	// Precompute the earliest-reading block per variable in a single pass over
	// ReadersByVar (O(total reads)). This replaces the per-block reader rescan
	// in the uninitialized-variable rule, which was O(readers²) per variable.
	// The tie-break is identical to the previous inline scan: lowest LineNumber
	// wins; on equal lines the first ID in ReadersByVar order is kept.
	ctx.FirstReaderByVar = make(map[string]string, len(ctx.ReadersByVar))
	for vname, ids := range ctx.ReadersByVar {
		lowestLine := -1
		lowestID := ""
		for _, id := range ids {
			b := ctx.AllBlocks[id]
			if b == nil {
				continue
			}
			if lowestLine < 0 || b.LineNumber < lowestLine {
				lowestLine = b.LineNumber
				lowestID = id
			}
		}
		ctx.FirstReaderByVar[vname] = lowestID
	}

	// Rule-specific lookups, each built in one O(blocks) pass to replace a
	// per-block full scan inside the corresponding rule (see field docs).
	ctx.LabelNameCount = make(map[string]int)
	ctx.LabelByName = buildLabelIndex(ctx)
	ctx.ClosedResourceVars = buildClosedResourceVars(ctx)

	// Index subflows by ID and name in a single pass (see field docs). First
	// match wins on duplicate names, preserving the prior inline scans' semantics.
	ctx.SubflowByID = make(map[string]*models.Subflow, len(flow.Subflows))
	ctx.SubflowByName = make(map[string]*models.Subflow, len(flow.Subflows))
	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		ctx.SubflowByID[sf.ID] = sf
		if _, ok := ctx.SubflowByName[sf.Name]; !ok {
			ctx.SubflowByName[sf.Name] = sf
		}
	}

	// Precompute ancestor-derived flags so rules do O(1) lookups instead of
	// re-walking parent chains + allocating visited maps on every block.
	ctx.HasErrorHandler = make(map[string]bool, len(ctx.AllBlocks))
	ctx.InsideRetryLoop = make(map[string]bool, len(ctx.AllBlocks))
	for id := range ctx.AllBlocks {
		ctx.computeHasErrorHandler(id)
		ctx.computeInsideRetryLoop(id)
	}

	// Precompute circular subflow dependencies (DFS on the call graph) so the
	// circular-subflow-dependency rule does an O(1) set lookup per subflow.
	ctx.CircularSubflows = make(map[string]bool)
	ctx.callGraph = buildCallGraph(flow)
	ctx.circularDeps = detectCircularDeps(flow, ctx.callGraph)
	for _, sfID := range ctx.circularDeps {
		ctx.CircularSubflows[sfID] = true
	}

	// Precompute per-subflow cyclomatic complexity and fan-in (mirror the call
	// graph inversion in ComputeFlowMetrics) so the high-cyclomatic and
	// uncalled-subflow rules do O(1) lookups.
	fanInByID := make(map[string]int, len(ctx.callGraph))
	for src, targets := range ctx.callGraph {
		for tgt := range targets {
			if tgt != src {
				fanInByID[tgt]++
			}
		}
	}
	ctx.fanInByID = fanInByID
	ctx.subflowMetrics = make([]models.SubflowMetrics, 0, len(flow.Subflows))
	ctx.SubflowCyclo = make(map[string]int, len(flow.Subflows))
	ctx.SubflowFanIn = make(map[string]int, len(flow.Subflows))
	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		m := computeSubflowMetrics(sf, ctx.callGraph, fanInByID)
		ctx.subflowMetrics = append(ctx.subflowMetrics, m)
		ctx.SubflowCyclo[sf.ID] = m.CyclomaticComplexity
		ctx.SubflowFanIn[sf.ID] = m.FanIn
	}

	// Precompute duplicate subflow names so the duplicate-subflow-name rule
	// does an O(1) set lookup.
	ctx.DuplicateSubflowNames = make(map[string]bool)
	nameCount := make(map[string]int, len(flow.Subflows))
	for i := range flow.Subflows {
		nameCount[flow.Subflows[i].Name]++
	}
	for name, count := range nameCount {
		if count > 1 {
			ctx.DuplicateSubflowNames[name] = true
		}
	}

	return ctx
}

func collectBlocks(ctx *RuleContext, blocks []models.Block, parentID string, subflowID string, depth int) {
	ptrs := make([]*models.Block, 0, len(blocks))
	for i := range blocks {
		if blocks[i].Type == models.BlockTypeEnd {
			continue
		}
		ptrs = append(ptrs, &blocks[i])
	}

	// Use a unique key for each sibling group: parentID for nested blocks,
	// subflowID for top-level blocks. Without this, multiple subflows would
	// overwrite SiblingMap[""] and BlockIndex values from earlier subflows
	// would index out of range in the last subflow's sibling slice.
	siblingKey := parentID
	if siblingKey == "" {
		siblingKey = subflowID
	}

	terminatorIdx := -1
	for i, b := range ptrs {
		ctx.BlockIndex[b.ID] = i
		ctx.SiblingKey[b.ID] = siblingKey
		if terminatorIdx < 0 && isTerminator(b) {
			terminatorIdx = i
		}

		ctx.AllBlocks[b.ID] = b
		ctx.BlocksByName[b.Name] = append(ctx.BlocksByName[b.Name], b)
		ctx.BlocksByType[b.Type] = append(ctx.BlocksByType[b.Type], b)
		if parentID != "" {
			ctx.ParentMap[b.ID] = parentID
		}
		ctx.BlockDepth[b.ID] = depth
		ctx.totalBlocks++

		// Extract used variables
		if b.Properties != nil {
			for _, val := range b.Properties {
				matches := variableRefRegex.FindAllStringSubmatch(val, -1)
				for _, m := range matches {
					if len(m) > 1 {
						ctx.UsedVariables[strings.TrimSpace(m[1])] = true
					}
				}
			}
		}
		if b.Variables != nil {
			for _, v := range b.Variables {
				matches := variableRefRegex.FindAllStringSubmatch(v, -1)
				for _, m := range matches {
					if len(m) > 1 {
						ctx.UsedVariables[strings.TrimSpace(m[1])] = true
					}
				}
			}
		}

		// Index variable writers (union of the _output and _var properties) and
		// readers (block.Variables entries) for O(1) data-flow lookups. Built in
		// traversal order so derived results are deterministic.
		if b.Properties != nil {
			if out := b.Properties["_output"]; out != "" {
				ctx.WritersByVar[out] = append(ctx.WritersByVar[out], b.ID)
			}
			if v := b.Properties["_var"]; v != "" {
				ctx.WritersByVar[v] = append(ctx.WritersByVar[v], b.ID)
			}
		}
		for _, v := range b.Variables {
			ctx.ReadersByVar[v] = append(ctx.ReadersByVar[v], b.ID)
		}
	}
	ctx.SiblingMap[siblingKey] = ptrs
	if terminatorIdx >= 0 {
		ctx.TerminatorIndex[siblingKey] = terminatorIdx
	}

	for i := range blocks {
		b := &blocks[i]
		if len(b.Children) > 0 {
			collectBlocks(ctx, b.Children, b.ID, subflowID, depth+1)
		}
	}
}

func isTerminator(b *models.Block) bool {
	if b.Type == models.BlockTypeEnd {
		return true
	}
	return matchesAny(b, terminatorNames)
}

func walkBlocks(flow *models.FlowDocument, fn func(block *models.Block)) {
	for i := range flow.Subflows {
		walkSubflowBlocks(&flow.Subflows[i], fn)
	}
}

func walkSubflowBlocks(sf *models.Subflow, fn func(block *models.Block)) {
	for i := range sf.Blocks {
		walkBlockTree(&sf.Blocks[i], fn)
	}
}

func walkBlockTree(b *models.Block, fn func(block *models.Block)) {
	fn(b)
	for i := range b.Children {
		walkBlockTree(&b.Children[i], fn)
	}
}

func computeStats(findings []models.Finding) models.AnalysisStats {
	stats := models.AnalysisStats{}
	for _, f := range findings {
		switch f.Severity {
		case models.SeverityError:
			stats.Errors++
		case models.SeverityWarning:
			stats.Warnings++
		case models.SeverityInfo:
			stats.Info++
		}
	}
	return stats
}

// safeCheck runs a single rule against a block, recovering from any panic so that
// one buggy rule (or a malformed block) can't abort the entire analysis. The
// offending rule/block is logged and skipped; the returned `skipped` flag lets
// the caller tally RulesSkipped on the report so operators can see that
// findings may be missing.
func safeCheck(rule Rule, block *models.Block, ctx *RuleContext) (findings []models.Finding, skipped bool) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("analysis rule panicked; skipping",
				"rule", rule.ID(), "block", block.ID, "panic", r)
			findings, skipped = nil, true
		}
	}()
	return rule.Check(block, ctx), false
}

// RunAnalysis runs all enabled rules over flow and collects per-rule timing in
// the returned RuleProfiles. The cached hot path (CachedAnalysis) calls the
// core with profiling disabled to avoid two time.Now() calls per (block, rule),
// which on some platforms are syscalls.
func RunAnalysis(flow *models.FlowDocument, rules []Rule, settings *models.AppSettings,
	onProgress func(current, total int, ruleName string)) *models.AnalysisReport {
	return RunAnalysisCtx(context.Background(), flow, rules, settings, onProgress)
}

// RunAnalysisCtx is the context-aware variant of RunAnalysis: the walk skips
// rule work once gctx is cancelled, bounding CPU for callers that pass a
// deadline (e.g. the raw-analyze endpoint's per-request timeout).
func RunAnalysisCtx(gctx context.Context, flow *models.FlowDocument, rules []Rule, settings *models.AppSettings,
	onProgress func(current, total int, ruleName string)) *models.AnalysisReport {
	return runAnalysisCore(gctx, flow, rules, settings, onProgress, true)
}

func runAnalysisCore(gctx context.Context, flow *models.FlowDocument, rules []Rule, settings *models.AppSettings,
	onProgress func(current, total int, ruleName string), profile bool) *models.AnalysisReport {

	start := time.Now()
	ctx := buildContext(flow, settings)

	var enabledRules []Rule
	for _, r := range rules {
		if settings != nil {
			if rc, ok := settings.Analysis.Rules[r.ID()]; ok {
				if !rc.Enabled {
					continue
				}
			}
		}
		enabledRules = append(enabledRules, r)
	}

	total := len(enabledRules) * ctx.totalBlocks
	counter := 0

	// Pre-resolve each rule's severity override once (instead of a per-finding
	// settings map lookup).
	severityOverride := make([]models.Severity, len(enabledRules))
	if settings != nil {
		for i, rule := range enabledRules {
			if rc, ok := settings.Analysis.Rules[rule.ID()]; ok && rc.Severity != "" {
				severityOverride[i] = models.Severity(rc.Severity)
			}
		}
	}

	// Walk the block tree ONCE and dispatch every enabled rule per block,
	// instead of doing a full traversal per rule (O(blocks) vs O(rules×blocks)).
	// Findings are bucketed per rule and flattened in rule order afterwards, so
	// the output order is byte-identical to the previous rule-major loop.
	buckets := make([][]models.Finding, len(enabledRules))
	ruleTimers := make([]int64, len(enabledRules))
	ruleBlocks := make([]int, len(enabledRules))
	rulesSkipped := 0

	walkBlocks(flow, func(block *models.Block) {
		if block.Type == models.BlockTypeEnd {
			return
		}
		// Respect request cancellation: a pathological payload on the raw-analyze
		// endpoint can keep the analyzer walking long after the client
		// disconnected (or the per-request deadline elapsed). Skip the rule work
		// for remaining blocks so the CPU burn stops; the partial report is
		// discarded upstream (the cancelled response is never written).
		if gctx != nil && gctx.Err() != nil {
			return
		}
		for i, rule := range enabledRules {
			counter++
			if onProgress != nil && counter%50 == 0 {
				onProgress(counter, total, rule.Name())
			}

			var t0 time.Time
			if profile {
				t0 = time.Now()
			}
			ruleFindings, skipped := safeCheck(rule, block, ctx)
			if skipped {
				rulesSkipped++
			}
			if profile {
				ruleTimers[i] += time.Since(t0).Microseconds()
			}
			ruleBlocks[i]++

			if ov := severityOverride[i]; ov != "" {
				for j := range ruleFindings {
					ruleFindings[j].Severity = ov
				}
			}
			if rule.Category() != "" {
				for j := range ruleFindings {
					if ruleFindings[j].Category == "" {
						ruleFindings[j].Category = rule.Category()
					}
				}
			}
			buckets[i] = append(buckets[i], ruleFindings...)
		}
	})

	var findings []models.Finding
	for i := range buckets {
		findings = append(findings, buckets[i]...)
	}
	if onProgress != nil && len(enabledRules) > 0 {
		onProgress(counter, total, enabledRules[len(enabledRules)-1].Name())
	}

	// Fallback for parse-error findings: the parse-error rule anchors on the
	// first block of the first subflow, so when no block is parseable (or the
	// first subflow has zero blocks), the per-block Check never fires. Emit
	// them here as a document-level fallback — but only if (a) the parse-error
	// rule is enabled and (b) no parse-error findings were already produced
	// by the per-block rule (avoids duplicates).
	if len(flow.ParseErrors) > 0 {
		// Respect the rule's enabled/disabled setting.
		peEnabled := true
		if settings != nil {
			if rc, ok := settings.Analysis.Rules["parse-error"]; ok {
				peEnabled = rc.Enabled
			}
		}
		if peEnabled {
			hasParseErrors := false
			for _, f := range findings {
				if f.RuleID == "parse-error" {
					hasParseErrors = true
					break
				}
			}
			if !hasParseErrors {
				anchorID := ""
				anchorSF := ""
				if len(flow.Subflows) > 0 {
					anchorSF = flow.Subflows[0].ID
				}
				for _, pe := range flow.ParseErrors {
					sev := models.SeverityError
					switch pe.Severity {
					case "warning":
						sev = models.SeverityWarning
					case "info":
						sev = models.SeverityInfo
					}
					findings = append(findings, models.Finding{
						RuleID:      "parse-error",
						Severity:    sev,
						Title:       "Parse error (line " + itoa(pe.Line) + ")",
						Description: pe.Message,
						BlockID:     anchorID,
						SubflowID:   anchorSF,
						Suggestion:  "Fix the syntax error so the parser can produce a complete block tree.",
						Metadata:    map[string]interface{}{"line": pe.Line, "column": pe.Column, "snippet": pe.Snippet},
					})
				}
			}
		}
	}

	if findings == nil {
		findings = []models.Finding{}
	}

	// Honor inline `# pad-ignore` directives in the flow source before stats,
	// metrics, IDs and fingerprints are derived, so a suppressed finding is
	// invisible to every downstream consumer (UI, CLI gate, baselines, SARIF).
	findings, suppressedCount := applyInlineSuppressions(findings, collectInlineSuppressions(flow))

	// Fold same-block, same-subject duplicates (e.g. a rule firing twice on one
	// block for the same variable) BEFORE stats/IDs/fingerprints, so every
	// downstream consumer (health score, diff, SARIF, chat context) sees the
	// de-duplicated set. The per-block groups are exposed on the report for the
	// UI's "N similar" affordance.
	findings, groups := DeduplicateFindings(findings)

	stats := computeStats(findings)
	stats.BlocksAnalyzed = ctx.totalBlocks
	stats.RulesRun = len(enabledRules)
	stats.RulesSkipped = rulesSkipped
	stats.Suppressed = suppressedCount

	elapsed := time.Since(start)
	report := &models.AnalysisReport{
		FlowID:      flow.ID,
		FlowName:    flow.Name,
		GeneratedAt: time.Now(),
		Findings:    findings,
		Groups:      groups,
		Stats:       stats,
		DurationMs:  int(elapsed.Milliseconds()),
	}

	for i := range findings {
		findings[i].ID = fmt.Sprintf("F-%03d", i+1)
		// Fingerprint is content-derived (stable across re-imports/re-parses),
		// not the legacy RuleID:BlockID, so triage/baseline/diff/SARIF keys
		// survive a desktop re-import or a CLI re-run. Key() is retained for
		// in-run uniqueness and legacy matching.
		findings[i].Fingerprint = findingContentKey(findings[i], flow)
		// Stamp a per-rule confidence default if the rule didn't set one, so
		// every finding carries a certainty for severity×confidence ordering.
		if findings[i].Confidence == "" {
			if c, ok := ruleConfidence[findings[i].RuleID]; ok {
				findings[i].Confidence = c
			} else {
				findings[i].Confidence = models.ConfidenceMedium
			}
		}
		// Stamp the available one-click auto-fix (if any) so the UI can show
		// "Apply fix" only where a deterministic fix exists.
		if findings[i].AutoFix == "" {
			findings[i].AutoFix = ruleAutoFix[findings[i].RuleID]
		}
	}

	report.Metrics = ComputeFlowMetricsFromCtx(ctx, report)

	profiles := make([]models.RuleProfile, len(enabledRules))
	for i, rule := range enabledRules {
		profiles[i] = models.RuleProfile{
			RuleID:        rule.ID(),
			RuleName:      rule.Name(),
			DurationMs:    ruleTimers[i] / 1000,
			FindingCount:  len(buckets[i]),
			BlocksChecked: ruleBlocks[i],
		}
	}
	report.RuleProfiles = profiles

	return report
}

func GetSiblings(ctx *RuleContext, block *models.Block) []*models.Block {
	key := ctx.SiblingKey[block.ID]
	return ctx.SiblingMap[key]
}

func GetParent(ctx *RuleContext, block *models.Block) *models.Block {
	pid, ok := ctx.ParentMap[block.ID]
	if !ok {
		return nil
	}
	return ctx.AllBlocks[pid]
}

// computeHasErrorHandler memoizes into ctx.HasErrorHandler. A block "has an
// error-handler ancestor" if any ancestor is itself an error handler or has
// an error-handler sibling.
func (ctx *RuleContext) computeHasErrorHandler(blockID string) bool {
	if v, ok := ctx.HasErrorHandler[blockID]; ok {
		return v
	}
	ctx.HasErrorHandler[blockID] = false // guard against cycles
	pid, ok := ctx.ParentMap[blockID]
	if !ok {
		return false
	}
	parent := ctx.AllBlocks[pid]
	if parent == nil {
		return false
	}
	direct := parent.Type == models.BlockTypeErrorHandler
	if !direct {
		for _, s := range ctx.SiblingMap[pid] {
			if s.Type == models.BlockTypeErrorHandler {
				direct = true
				break
			}
		}
	}
	result := direct || ctx.computeHasErrorHandler(pid)
	ctx.HasErrorHandler[blockID] = result
	return result
}

// computeInsideRetryLoop memoizes into ctx.InsideRetryLoop. A block is "inside
// a retry loop" if any ancestor is a LOOP whose name or properties include
// "retry" or "attempt".
func (ctx *RuleContext) computeInsideRetryLoop(blockID string) bool {
	if v, ok := ctx.InsideRetryLoop[blockID]; ok {
		return v
	}
	ctx.InsideRetryLoop[blockID] = false // guard against cycles
	pid, ok := ctx.ParentMap[blockID]
	if !ok {
		return false
	}
	parent := ctx.AllBlocks[pid]
	if parent == nil {
		return false
	}
	isRetry := false
	if parent.Type == models.BlockTypeLoop {
		nameLower := strings.ToLower(parent.Name)
		isRetry = strings.Contains(nameLower, "retry") || strings.Contains(nameLower, "attempt")
		for k, v := range parent.Properties {
			kl := strings.ToLower(k)
			if (strings.Contains(kl, "retry") || strings.Contains(kl, "attempt")) && v != "" {
				isRetry = true
			}
		}
	}
	result := isRetry || ctx.computeInsideRetryLoop(pid)
	ctx.InsideRetryLoop[blockID] = result
	return result
}

func HasErrorHandlerAncestor(ctx *RuleContext, block *models.Block) bool {
	return ctx.HasErrorHandler[block.ID]
}

func BuildVariableLineage(doc *models.FlowDocument, varName string) *models.VariableHistory {
	history := &models.VariableHistory{
		Name:   varName,
		Events: make([]models.VariableEvent, 0),
	}

	if doc == nil || varName == "" {
		return history
	}

	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		walkSubflowBlocks(sf, func(b *models.Block) {
			// Check for assignment/output
			if b.Properties != nil {
				if output, ok := b.Properties["_output"]; ok && output == varName {
					eventType := "init"
					if b.RawType != "SET" && b.RawType != "Variables.SetVariable" {
						eventType = "mutate"
					}
					history.Events = append(history.Events, models.VariableEvent{
						Type:      eventType,
						BlockID:   b.ID,
						Line:      b.LineNumber,
						SubflowID: sf.Name,
					})
					return // If it's an output, we count it once (even if it also reads itself, e.g. x = x + 1)
				}
			}

			// Check for read
			if b.Variables != nil {
				for _, v := range b.Variables {
					if v == varName {
						history.Events = append(history.Events, models.VariableEvent{
							Type:      "read",
							BlockID:   b.ID,
							Line:      b.LineNumber,
							SubflowID: sf.Name,
						})
						break
					}
				}
			}
		})
	}

	return history
}
