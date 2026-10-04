package analyzer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pad-core/logger"
	"pad-core/models"
)

var variableRefRegex = regexp.MustCompile(`%([^%]+)%`)

// subjectMetaKeys name a finding's subject for content keys; dedup.go's subjectKeys aliases this slice.
var subjectMetaKeys = []string{"variable", "property", "resource"}

// ruleConfidence is the per-rule default certainty; unlisted rules default to Medium.
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

// ruleAutoFix maps rule to its one-click fixType; keep in sync with FlowService.ApplyFix.
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
	"duplicate-action":         "remove-block",
	"redundant-action":         "remove-block",
	"dead-code":                "remove-block",
	"dead-data":                "remove-block",
	"empty-branch":             "remove-block",
	// empty-subflow and uncalled-subflow have NO auto-fix: remove-block would
	// strip one action per iteration, and subflows may be invoked dynamically.
	"unused-variable":          "remove-block",
	"disabled-block":           "remove-block",
	"wait-zero":                "remove-block",
	"todo-in-comment":          "remove-block",
	"hardcoded-credential":     "replace-with-variable",
	"hardcoded-filepath":       "replace-with-variable",
	"hardcoded-url":            "replace-with-variable",
	"hardcoded-ip":             "replace-with-variable",
	"sensitive-exposure":       "mask-sensitive-variable",
	"switch-no-default":        "insert-default",
	"slow-pattern":             "insert-delay-in-loop",
	"sql-injection-risk":       "parameterize-sql",
	"insecure-http-url":        "upgrade-to-https",
	"hardcoded-ui-coordinates": "replace-with-variable",
	"command-injection-risk":   "sanitize-command-vars",
}

// RuleConfidence returns a rule's built-in default confidence, or Medium when unlisted.
func RuleConfidence(id string) models.Confidence {
	if c, ok := ruleConfidence[id]; ok {
		return c
	}
	return models.ConfidenceMedium
}

// RuleAutoFix returns the rule's one-click fixType, or "" when none; keep in sync with FlowService.ApplyFix.
func RuleAutoFix(id string) string {
	return ruleAutoFix[id]
}

// directAutoFixRuleIDs: rules stamping AutoFix in Check instead of ruleAutoFix; unlisted ones never get fixed.
var directAutoFixRuleIDs = map[string]bool{
	"subflow-mismatch": true, // "append-output" set in Check
}

func mayEmitAutoFix(r Rule) bool {
	if ruleAutoFix[r.ID()] != "" || directAutoFixRuleIDs[r.ID()] {
		return true
	}
	if cr, ok := r.(*CustomRule); ok {
		return cr.config.AutoFix != ""
	}
	return false
}

// findingContentKey derives identity from block CONTENT (not parser-minted
// UUIDs, which change on every re-parse) so fingerprint keys survive re-imports.
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
	// WritersByVar/ReadersByVar map variable to block IDs writing (_output/_var) or reading (Variables) it.
	WritersByVar map[string][]string
	ReadersByVar map[string][]string
	// FirstReaderByVar maps variable to its earliest-reading block ID (lowest LineNumber).
	FirstReaderByVar map[string]string
	// LabelByName maps a lowercased label name to its document-order-first LABEL block.
	LabelByName map[string]*models.Block
	// LabelNameCount counts LABEL blocks per lowercased name (duplicate-label).
	LabelNameCount map[string]int
	// ClosedResourceVars maps close-action prefix to variables referenced by any closing block.
	ClosedResourceVars map[string]map[string]bool
	// SubflowByID / SubflowByName resolve subflows; on duplicate names the first in document order wins.
	SubflowByID   map[string]*models.Subflow
	SubflowByName map[string]*models.Subflow
	// sigCache memoizes block content signatures so duplicate-action hashes each block once.
	sigCache    map[string]string
	totalBlocks int
	// HasErrorHandler marks blocks whose ancestor chain contains an error-handler sibling.
	HasErrorHandler map[string]bool
	// GroupHasHandler marks sibling-group keys containing an error-handler block.
	GroupHasHandler map[string]bool
	// AllReadersDead marks variables whose readers are all unreachable (dead-data).
	AllReadersDead map[string]bool
	// TaintedVars: variables tracing to an untrusted source; fixed point over WritersByVar.
	TaintedVars map[string]bool
	// retryLoopParent memoizes the parentIsRetryLoop verdict per LOOP block ID.
	retryLoopParent map[string]bool
	// InsideRetryLoop marks blocks enclosed by a retry/attempt LOOP (missing-retry).
	InsideRetryLoop map[string]bool
	// CircularSubflows marks subflow IDs on a call-graph cycle.
	CircularSubflows map[string]bool
	// SubflowCyclo maps a subflow ID to its cyclomatic complexity.
	SubflowCyclo map[string]int
	// SubflowFanIn maps a subflow ID to its distinct-caller count (uncalled-subflow).
	SubflowFanIn map[string]int
	// DuplicateSubflowNames is the set of subflow names appearing more than once.
	DuplicateSubflowNames map[string]bool
	// DescendantCount maps block ID to non-End/non-Comment blocks in its subtree, post-order.
	DescendantCount map[string]int

	// Graph/metrics artifacts reused by the report's metrics phase.
	callGraph      map[string]map[string]bool
	fanInByID      map[string]int
	circularDeps   []string
	subflowMetrics []models.SubflowMetrics
}

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

// buildIndexContext builds the structural indexes; buildContext layers rule-only ones (nil-map reads safe).
func buildIndexContext(flow *models.FlowDocument, settings *models.AppSettings) *RuleContext {
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
		DescendantCount: make(map[string]int),
		GroupHasHandler: make(map[string]bool),
		Settings:        settings,
	}

	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		collectBlocks(ctx, sf.Blocks, "", sf.ID, 0)
	}

	// Earliest reader per variable in one O(total reads) pass; tie-break:
	// lowest LineNumber, then first ID in ReadersByVar order.
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

	return ctx
}

func buildContext(flow *models.FlowDocument, settings *models.AppSettings) *RuleContext {
	ctx := buildIndexContext(flow, settings)
	ctx.sigCache = make(map[string]string, len(ctx.AllBlocks))

	// Dead-data lookup skipped when no group has a terminator; the rule early-returns then.
	if len(ctx.TerminatorIndex) > 0 {
		ctx.AllReadersDead = buildAllReadersDead(ctx)
	}

	ctx.LabelNameCount = make(map[string]int)
	ctx.LabelByName = buildLabelIndex(ctx)
	ctx.ClosedResourceVars = buildClosedResourceVars(ctx)
	ctx.TaintedVars = buildTaintedVars(ctx)

	ctx.SubflowByID = make(map[string]*models.Subflow, len(flow.Subflows))
	ctx.SubflowByName = make(map[string]*models.Subflow, len(flow.Subflows))
	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		ctx.SubflowByID[sf.ID] = sf
		if _, ok := ctx.SubflowByName[sf.Name]; !ok {
			ctx.SubflowByName[sf.Name] = sf
		}
	}

	ctx.HasErrorHandler = make(map[string]bool, len(ctx.AllBlocks))
	ctx.InsideRetryLoop = make(map[string]bool, len(ctx.AllBlocks))
	ctx.retryLoopParent = make(map[string]bool)
	for id := range ctx.AllBlocks {
		ctx.computeHasErrorHandler(id)
		ctx.computeInsideRetryLoop(id)
	}

	ctx.CircularSubflows = make(map[string]bool)
	ctx.callGraph = buildCallGraph(flow)
	ctx.circularDeps = detectCircularDeps(flow, ctx.callGraph)
	for _, sfID := range ctx.circularDeps {
		ctx.CircularSubflows[sfID] = true
	}

	// Per-subflow complexity and fan-in; mirrors ComputeFlowMetrics' inversion.
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

	// Pre-fill all signatures: the rule walk is sharded across workers, so
	// this makes every rule's ctx access a pure read.
	for _, b := range ctx.AllBlocks {
		ctx.blockSig(b)
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

	// Sibling groups key by parentID (nested) or subflowID (top level): sharing
	// "" would overwrite SiblingMap[""] across subflows and index out of range.
	siblingKey := parentID
	if siblingKey == "" {
		siblingKey = subflowID
	}

	terminatorIdx := -1
	groupHasHandler := false
	for i, b := range ptrs {
		ctx.BlockIndex[b.ID] = i
		ctx.SiblingKey[b.ID] = siblingKey
		if terminatorIdx < 0 && isTerminator(b) {
			terminatorIdx = i
		}
		if b.Type == models.BlockTypeErrorHandler {
			groupHasHandler = true
		}

		ctx.AllBlocks[b.ID] = b
		ctx.BlocksByName[b.Name] = append(ctx.BlocksByName[b.Name], b)
		ctx.BlocksByType[b.Type] = append(ctx.BlocksByType[b.Type], b)
		if parentID != "" {
			ctx.ParentMap[b.ID] = parentID
		}
		ctx.BlockDepth[b.ID] = depth
		ctx.totalBlocks++

		if b.Properties != nil {
			for _, val := range b.Properties {
				if !strings.Contains(val, "%") {
					continue // %var% is the only shape variableRefRegex matches
				}
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
				if !strings.Contains(v, "%") {
					continue
				}
				matches := variableRefRegex.FindAllStringSubmatch(v, -1)
				for _, m := range matches {
					if len(m) > 1 {
						ctx.UsedVariables[strings.TrimSpace(m[1])] = true
					}
				}
			}
		}

		// Index variable writers/readers in traversal order so derived results are deterministic.
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
	if groupHasHandler {
		ctx.GroupHasHandler[siblingKey] = true
	}

	for i := range blocks {
		b := &blocks[i]
		if len(b.Children) > 0 {
			collectBlocks(ctx, b.Children, b.ID, subflowID, depth+1)
		}
	}

	// Post-order descendant counts credited to the parent of this child-group.
	subtree := 0
	for i := range blocks {
		b := &blocks[i]
		if b.Type != models.BlockTypeEnd && b.Type != models.BlockTypeComment {
			subtree++
		}
		subtree += ctx.DescendantCount[b.ID]
	}
	if parentID != "" {
		ctx.DescendantCount[parentID] = subtree
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

// parseErrorRuleEnabled reports whether settings leave the parse-error rule
// enabled; the suppression inventory must mirror the engine's fallback exactly.
func parseErrorRuleEnabled(settings *models.AppSettings) bool {
	if settings != nil {
		if rc, ok := settings.Analysis.Rules["parse-error"]; ok {
			return rc.Enabled
		}
	}
	return true
}

// safeCheck recovers rule panics so one buggy rule cannot abort the analysis; skipped feeds RulesSkipped.
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

// RunAnalysis runs all enabled rules with per-rule profiling; cached hot path uses RunAnalysisCtxNoProfile.
func RunAnalysis(flow *models.FlowDocument, rules []Rule, settings *models.AppSettings,
	onProgress func(current, total int, ruleName string)) *models.AnalysisReport {
	return RunAnalysisCtx(context.Background(), flow, rules, settings, onProgress)
}

// RunAnalysisCtx is RunAnalysis with cancellation, bounding CPU for deadline-bound callers.
func RunAnalysisCtx(gctx context.Context, flow *models.FlowDocument, rules []Rule, settings *models.AppSettings,
	onProgress func(current, total int, ruleName string)) *models.AnalysisReport {
	return runAnalysisCore(gctx, flow, rules, settings, onProgress, true)
}

// RunAnalysisCtxNoProfile drops per-(block, rule) probes: the apply-fix loop re-analyzes each iteration.
func RunAnalysisCtxNoProfile(gctx context.Context, flow *models.FlowDocument, rules []Rule, settings *models.AppSettings,
	onProgress func(current, total int, ruleName string)) *models.AnalysisReport {
	return runAnalysisCore(gctx, flow, rules, settings, onProgress, false)
}

// runAnalysisCore returns a partial report on cancellation; anything that caches
// or records a report must use runAnalysisWalk and discard an incomplete one.
func runAnalysisCore(gctx context.Context, flow *models.FlowDocument, rules []Rule, settings *models.AppSettings,
	onProgress func(current, total int, ruleName string), profile bool) *models.AnalysisReport {
	report, _ := runAnalysisWalk(gctx, flow, rules, settings, onProgress, profile)
	return report
}

const progressFlushEvery = 50

// runAnalysisWalk runs every enabled rule; complete is false when cancelled
// early, and the truncated report must not be cached or recorded.
func runAnalysisWalk(gctx context.Context, flow *models.FlowDocument, rules []Rule, settings *models.AppSettings,
	onProgress func(current, total int, ruleName string), profile bool) (_ *models.AnalysisReport, complete bool) {

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

	// Pre-resolve severity overrides once instead of per-finding lookups.
	severityOverride := make([]models.Severity, len(enabledRules))
	if settings != nil {
		for i, rule := range enabledRules {
			if rc, ok := settings.Analysis.Rules[rule.ID()]; ok && rc.Severity != "" {
				severityOverride[i] = models.Severity(rc.Severity)
			}
		}
	}

	// Parallel shards (ctx read-only; rules stateless); concatenating buckets in
	// order preserves the sequential rule-major, document-order sequence.
	blocks := make([]*models.Block, 0, ctx.totalBlocks)
	walkBlocks(flow, func(block *models.Block) {
		if block.Type != models.BlockTypeEnd {
			blocks = append(blocks, block)
		}
	})

	workers := runtime.GOMAXPROCS(0)
	// Match the batch pool's ceiling: concurrent full GOMAXPROCS fan-outs would oversubscribe.
	if workers > 8 {
		workers = 8
	}
	if len(blocks) < 256 {
		// Sharding overhead buys nothing on small flows.
		workers = 1
	}
	if workers > len(blocks) {
		workers = len(blocks)
	}
	if workers < 1 {
		// Zero-block docs: the shard loop is a no-op, but shardSize needs a positive divisor.
		workers = 1
	}

	type shardResult struct {
		buckets      [][]models.Finding
		ruleTimers   []int64
		ruleBlocks   []int
		rulesSkipped int
		counter      int
		// aborted: the shard stopped early on a cancelled context.
		aborted bool
	}
	// Allocated up front so the aggregation below is safe even when no shard runs.
	results := make([]shardResult, workers)
	for w := range results {
		results[w].buckets = make([][]models.Finding, len(enabledRules))
		results[w].ruleTimers = make([]int64, len(enabledRules))
		results[w].ruleBlocks = make([]int, len(enabledRules))
	}
	// Progress fires only when the integer pct advances (per-batch reporting would
	// hit progressMu and the SSE bus lock thousands of times); workers must not interleave.
	var progressMu sync.Mutex
	var done, lastPct atomic.Int64
	lastPct.Store(-1)
	reportProgress := func(batch int, ruleName string) {
		n := done.Add(int64(batch))
		pct := n * 100 / int64(total)
		// 100% is reserved for the final call after aggregation.
		if pct >= 100 || pct <= lastPct.Load() {
			return
		}
		progressMu.Lock()
		if pct > lastPct.Load() {
			lastPct.Store(pct)
			onProgress(int(n), total, ruleName)
		}
		progressMu.Unlock()
	}

	runShard := func(res *shardResult, shardBlocks []*models.Block) {
		pending := 0
		for _, block := range shardBlocks {
			// Stop the shard on cancellation and flag it so the report is treated as truncated.
			if gctx != nil && gctx.Err() != nil {
				res.aborted = true
				return
			}
			for i, rule := range enabledRules {
				res.counter++
				if onProgress != nil {
					pending++
					if pending == progressFlushEvery {
						reportProgress(pending, rule.Name())
						pending = 0
					}
				}

				var t0 time.Time
				if profile {
					t0 = time.Now()
				}
				ruleFindings, skipped := safeCheck(rule, block, ctx)
				if skipped {
					res.rulesSkipped++
				}
				if profile {
					res.ruleTimers[i] += time.Since(t0).Microseconds()
				}
				res.ruleBlocks[i]++

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
				res.buckets[i] = append(res.buckets[i], ruleFindings...)
			}
		}
	}

	shardSize := (len(blocks) + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := w * shardSize
		hi := lo + shardSize
		if hi > len(blocks) {
			hi = len(blocks)
		}
		if lo >= hi {
			break
		}
		wg.Add(1)
		go func(w int, shardBlocks []*models.Block) {
			defer wg.Done()
			runShard(&results[w], shardBlocks)
		}(w, blocks[lo:hi])
	}
	wg.Wait()

	// Aggregate rule-major: findings order = (rule, document order); timers
	// and counters summed across shards.
	var findings []models.Finding
	ruleTimers := make([]int64, len(enabledRules))
	ruleBlocks := make([]int, len(enabledRules))
	ruleFindingCounts := make([]int, len(enabledRules))
	rulesSkipped, counter := 0, 0
	complete = true
	for i := range enabledRules {
		for w := range results {
			findings = append(findings, results[w].buckets[i]...)
			ruleTimers[i] += results[w].ruleTimers[i]
			ruleBlocks[i] += results[w].ruleBlocks[i]
			ruleFindingCounts[i] += len(results[w].buckets[i])
		}
	}
	for w := range results {
		rulesSkipped += results[w].rulesSkipped
		counter += results[w].counter
		if results[w].aborted {
			complete = false
		}
	}
	if onProgress != nil && len(enabledRules) > 0 {
		onProgress(counter, total, enabledRules[len(enabledRules)-1].Name())
	}

	// Fallback when nothing parses: only when enabled and no parse-error findings exist yet.
	if len(flow.ParseErrors) > 0 {
		if parseErrorRuleEnabled(settings) {
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
				findings = append(findings, buildParseErrorFindings(flow.ParseErrors, anchorID, anchorSF)...)
			}
		}
	}

	if findings == nil {
		findings = []models.Finding{}
	}

	// Honor inline `# pad-ignore` directives before stats, metrics, IDs and
	// fingerprints are derived so a suppressed finding is invisible downstream.
	findings, suppressedCount := applyInlineSuppressions(findings, collectInlineSuppressions(flow))

	// Fold same-block, same-subject duplicates before stats; per-block groups
	// are NOT built here — the dedup endpoint computes them on demand.
	keep := dedupKeep(findings)
	stampFindings(findings, keep, flow)
	findings = keptFindings(findings, keep)

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
		Stats:       stats,
		DurationMs:  int(elapsed.Milliseconds()),
	}

	report.Metrics = ComputeFlowMetricsFromCtx(ctx, report)

	profiles := make([]models.RuleProfile, len(enabledRules))
	for i, rule := range enabledRules {
		profiles[i] = models.RuleProfile{
			RuleID:        rule.ID(),
			RuleName:      rule.Name(),
			DurationMs:    ruleTimers[i] / 1000,
			FindingCount:  ruleFindingCounts[i],
			BlocksChecked: ruleBlocks[i],
		}
	}
	report.RuleProfiles = profiles

	return report, complete
}

// stampFindings fills ID/Fingerprint/Confidence/AutoFix on kept findings only; IDs number them in dedup order.
func stampFindings(findings []models.Finding, keep []bool, flow *models.FlowDocument) {
	n := 0
	for i := range findings {
		if !keep[i] {
			continue
		}
		n++
		findings[i].ID = fmt.Sprintf("F-%03d", n)
		// Fingerprint is content-derived (stable across re-imports/re-parses);
		// Key() stays for in-run uniqueness and legacy matching.
		findings[i].Fingerprint = findingContentKey(findings[i], flow)
		// Stamp per-rule confidence default for severity×confidence ordering.
		if findings[i].Confidence == "" {
			if c, ok := ruleConfidence[findings[i].RuleID]; ok {
				findings[i].Confidence = c
			} else {
				findings[i].Confidence = models.ConfidenceMedium
			}
		}
		if findings[i].AutoFix == "" {
			findings[i].AutoFix = ruleAutoFix[findings[i].RuleID]
		}
	}
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

// computeHasErrorHandler memoizes: an ancestor is or has an error-handler sibling.
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
	// GroupHasHandler[pid] means "any sibling of this block is a handler"
	// (nested sibling groups key by the parent's ID).
	direct := parent.Type == models.BlockTypeErrorHandler || ctx.GroupHasHandler[pid]
	result := direct || ctx.computeHasErrorHandler(pid)
	ctx.HasErrorHandler[blockID] = result
	return result
}

// computeInsideRetryLoop memoizes: any ancestor is a LOOP whose name/properties include retry or attempt.
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
	isRetry, ok := ctx.retryLoopParent[pid]
	if !ok {
		isRetry = parentIsRetryLoop(parent)
		ctx.retryLoopParent[pid] = isRetry
	}
	result := isRetry || ctx.computeInsideRetryLoop(pid)
	ctx.InsideRetryLoop[blockID] = result
	return result
}

func parentIsRetryLoop(parent *models.Block) bool {
	if parent.Type != models.BlockTypeLoop {
		return false
	}
	nameLower := strings.ToLower(parent.Name)
	if strings.Contains(nameLower, "retry") || strings.Contains(nameLower, "attempt") {
		return true
	}
	for k, v := range parent.Properties {
		if v == "" {
			continue
		}
		kl := strings.ToLower(k)
		if strings.Contains(kl, "retry") || strings.Contains(kl, "attempt") {
			return true
		}
	}
	return false
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
					return // count an output once, even if it also reads itself (x = x + 1)
				}
			}

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
