package analyzer

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pad-core/logger"
	"pad-core/models"
)

// subjectMetaKeys name a finding's subject for content keys; dedup.go's subjectKeys aliases this slice.
var subjectMetaKeys = []string{"variable", "property", "resource"}

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
// enabled; the engine and the suppression inventory share parseErrorFallback.
func parseErrorRuleEnabled(settings *models.AppSettings) bool {
	return RuleEnabled(&ParseErrorRule{}, settings)
}

// parseErrorFallback is the document-level parse-error report: when the flow
// has parse errors, parse-error is enabled under settings and the walk produced
// no parse-error finding (no anchor block), the errors are reported block-less
// against the first subflow, whether or not parse-error is in the run's rule
// list (the custom-rule tester shows them).
func parseErrorFallback(flow *models.FlowDocument, settings *models.AppSettings, findings []models.Finding) []models.Finding {
	if len(flow.ParseErrors) == 0 || !parseErrorRuleEnabled(settings) {
		return nil
	}
	for i := range findings {
		if findings[i].RuleID == "parse-error" {
			return nil
		}
	}
	anchorSF := ""
	if len(flow.Subflows) > 0 {
		anchorSF = flow.Subflows[0].ID
	}
	out := buildParseErrorFindings(flow.ParseErrors, "", anchorSF)
	if ov := severityOverride(&ParseErrorRule{}, settings); ov != "" {
		for i := range out {
			out[i].Severity = ov
		}
	}
	return out
}

// severityOverride is the severity settings configure for r when it differs
// from r.DefaultSeverity(), else "": the seeded settings carry every rule's
// default, which must not flatten the severities a rule grades itself
// (parse-error, sql-injection-risk). Case and spacing are ignored; a value
// that is not error, warning or info overrides as error, so every gate fails
// closed on it as bakicli's does (nothing validates settings blobs on write).
func severityOverride(r Rule, settings *models.AppSettings) models.Severity {
	if settings == nil {
		return ""
	}
	rc, ok := settings.Analysis.Rules[r.ID()]
	if !ok {
		return ""
	}
	sev := NormalizeSeverity(rc.Severity)
	if sev == "" || sev == r.DefaultSeverity() {
		return ""
	}
	return sev
}

// safeCheck recovers rule panics so one buggy rule cannot abort the analysis; skipped feeds RulesSkipped.
func safeCheck(rule BlockRule, block *models.Block, ctx *RuleContext) (findings []models.Finding, skipped bool) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("analysis rule panicked; skipping",
				"rule", rule.ID(), "block", block.ID, "panic", r)
			findings, skipped = nil, true
		}
	}()
	return rule.Check(block, ctx), false
}

// safeCheckFlow is safeCheck for a flow rule.
func safeCheckFlow(rule FlowRule, ctx *RuleContext) (findings []models.Finding, skipped bool) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("analysis rule panicked; skipping", "rule", rule.ID(), "panic", r)
			findings, skipped = nil, true
		}
	}()
	return rule.CheckFlow(ctx), false
}

// unknownKindLogged holds the IDs of rules of unknown kind already logged.
var unknownKindLogged sync.Map

func logUnknownKind(r Rule) {
	if _, seen := unknownKindLogged.LoadOrStore(r.ID(), struct{}{}); !seen {
		logger.Error("analysis rule is neither a BlockRule nor a FlowRule; skipping",
			"rule", r.ID(), "type", fmt.Sprintf("%T", r))
	}
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
	return analyzeContext(gctx, ctx, start, rules, settings, onProgress, profile)
}

// analyzeContext is the walk over a built context: rule findings, the
// parse-error fallback, inline suppression, dedup, stamping, stats, metrics
// and rule profiles.
func analyzeContext(gctx context.Context, ctx *RuleContext, start time.Time, rules []Rule, settings *models.AppSettings,
	onProgress func(current, total int, ruleName string), profile bool) (_ *models.AnalysisReport, complete bool) {
	flow := ctx.Flow

	var enabledRules []Rule
	for _, r := range rules {
		if RuleEnabled(r, settings) {
			enabledRules = append(enabledRules, r)
		}
	}

	// Pre-resolve severity overrides once instead of per-finding lookups.
	var overrides []models.Severity
	if settings != nil {
		overrides = make([]models.Severity, len(enabledRules))
		for i, r := range enabledRules {
			overrides[i] = severityOverride(r, settings)
		}
	}

	walk := collectFindings(gctx, ctx, enabledRules, walkOptions{overrides: overrides, profile: profile, onProgress: onProgress})
	findings := append(walk.findings, parseErrorFallback(flow, settings, walk.findings)...)
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
	stats.RulesSkipped = walk.skipped
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
			DurationMs:    walk.timers[i] / 1000,
			FindingCount:  walk.counts[i],
			BlocksChecked: walk.checked[i],
		}
	}
	report.RuleProfiles = profiles
	report.RuleSet = RecordRuleSet(enabledRules)

	return report, walk.complete
}

// walkOptions is what a walk does beyond collecting findings.
type walkOptions struct {
	overrides  []models.Severity // per enabled rule, "" = keep; nil = none
	profile    bool
	onProgress func(current, total int, ruleName string)
	// visit, when set, receives every finding instead of walkResult.findings
	// (left nil): calls are serialized and in no particular order, so callers
	// that need only where rules fire skip the buckets, the flat copy and the
	// document-order sort.
	visit func(f *models.Finding)
}

// walkResult is one walk's raw (unsuppressed, undeduplicated) findings.
type walkResult struct {
	findings []models.Finding // rule-major in enabled order; document order within a rule
	counts   []int            // findings per enabled rule
	timers   []int64          // microseconds per enabled rule; zero unless profiled
	checked  []int            // BlocksChecked per enabled rule
	skipped  int              // recovered panics, plus one per rule of unknown kind
	complete bool             // false when cancelled
}

// collectFindings runs the enabled rules over ctx: the one dispatcher behind
// the analysis walk and the suppression inventory. Block rules run per non-END
// block in up to 8 contiguous shards; flow rules run once each on the calling
// goroutine while the shards run. The shards still step through every rule's
// slot per block, so a flow rule keeps its place in the rule order, its
// progress units and its BlocksChecked (a skipped rule of unknown kind keeps
// its progress units).
func collectFindings(gctx context.Context, ctx *RuleContext, enabled []Rule, opts walkOptions) walkResult {
	ctx.gctx = gctx
	n := len(enabled)
	res := walkResult{
		counts:   make([]int, n),
		timers:   make([]int64, n),
		checked:  make([]int, n),
		complete: true,
	}
	total := n * ctx.totalBlocks

	categories := make([]string, n)
	kinds := make([]ruleKind, n)
	blockRules := make([]BlockRule, n) // nil where enabled[i] is not a block rule
	// A custom rule under a built-in's ID: its findings are stamped stable, so
	// neither stampFindings nor a gate reads the built-in's metadata for them.
	shadowsBuiltin := make([]bool, n)
	var flowIdx []int
	for i, r := range enabled {
		categories[i] = r.Category()
		if _, custom := r.(*CustomRule); custom {
			shadowsBuiltin[i] = lookupMetaRef(r.ID()) != nil
		}
		kinds[i] = kindOf(r)
		switch kinds[i] {
		case kindBlock:
			blockRules[i] = r.(BlockRule)
		case kindFlow:
			flowIdx = append(flowIdx, i)
		default:
			logUnknownKind(r)
			res.skipped++
		}
	}

	// finish applies what the engine adds to every rule's findings.
	finish := func(i int, fs []models.Finding) {
		if opts.overrides != nil {
			if ov := opts.overrides[i]; ov != "" {
				for j := range fs {
					fs[j].Severity = ov
				}
			}
		}
		if c := categories[i]; c != "" {
			for j := range fs {
				if fs[j].Category == "" {
					fs[j].Category = c
				}
			}
		}
		if shadowsBuiltin[i] {
			for j := range fs {
				fs[j].Stage = string(StageStable)
			}
		}
	}
	var visitMu sync.Mutex
	visit := func(i int, fs []models.Finding) {
		visitMu.Lock()
		defer visitMu.Unlock()
		res.counts[i] += len(fs)
		for j := range fs {
			opts.visit(&fs[j])
		}
	}

	// Parallel shards (ctx read-only; rules stateless); concatenating buckets in
	// order preserves the sequential rule-major, document-order sequence.
	blocks := make([]*models.Block, 0, ctx.totalBlocks)
	walkBlocks(ctx.Flow, func(block *models.Block) {
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
		results[w].buckets = make([][]models.Finding, n)
		results[w].ruleTimers = make([]int64, n)
		results[w].ruleBlocks = make([]int, n)
	}
	// Progress fires only when the integer pct advances (per-batch reporting would
	// hit progressMu and the SSE bus lock thousands of times); workers must not interleave.
	var progressMu sync.Mutex
	var done, lastPct atomic.Int64
	lastPct.Store(-1)
	reportProgress := func(batch int, ruleName string) {
		cur := done.Add(int64(batch))
		pct := cur * 100 / int64(total)
		// 100% is reserved for the final call after aggregation.
		if pct >= 100 || pct <= lastPct.Load() {
			return
		}
		progressMu.Lock()
		if pct > lastPct.Load() {
			lastPct.Store(pct)
			opts.onProgress(int(cur), total, ruleName)
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
			for i, rule := range blockRules {
				res.counter++
				if opts.onProgress != nil {
					pending++
					if pending == progressFlushEvery {
						reportProgress(pending, enabled[i].Name())
						pending = 0
					}
				}
				if rule == nil {
					// A flow rule's check of this block happens in its one CheckFlow.
					if kinds[i] == kindFlow {
						res.ruleBlocks[i]++
					}
					continue
				}

				var t0 time.Time
				if opts.profile {
					t0 = time.Now()
				}
				ruleFindings, skipped := safeCheck(rule, block, ctx)
				if skipped {
					res.rulesSkipped++
				}
				if opts.profile {
					res.ruleTimers[i] += time.Since(t0).Microseconds()
				}
				res.ruleBlocks[i]++
				if len(ruleFindings) > 0 {
					finish(i, ruleFindings)
					if opts.visit != nil {
						visit(i, ruleFindings)
					} else {
						res.buckets[i] = append(res.buckets[i], ruleFindings...)
					}
				}
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

	// Flow rules, while the shards run.
	flowFindings := make([][]models.Finding, n)
	for _, i := range flowIdx {
		if gctx != nil && gctx.Err() != nil {
			res.complete = false
			break
		}
		rule := enabled[i].(FlowRule)
		var t0 time.Time
		if opts.profile {
			t0 = time.Now()
		}
		fs, skipped := safeCheckFlow(rule, ctx)
		if skipped {
			res.skipped++
		}
		if opts.profile {
			res.timers[i] = time.Since(t0).Microseconds()
		}
		finish(i, fs)
		if opts.visit != nil {
			visit(i, fs)
			continue
		}
		sortByDocPosition(ctx, fs)
		flowFindings[i] = fs
	}
	wg.Wait()

	// Aggregate rule-major: findings order = (rule, document order); timers
	// and counters summed across shards.
	counter, size := 0, 0
	for i := range enabled {
		c := len(flowFindings[i])
		for w := range results {
			c += len(results[w].buckets[i])
			res.timers[i] += results[w].ruleTimers[i]
			res.checked[i] += results[w].ruleBlocks[i]
		}
		res.counts[i] += c // visited findings were counted as they were handed over
		size += c
	}
	if size > 0 {
		res.findings = make([]models.Finding, 0, size)
		for i := range enabled {
			res.findings = append(res.findings, flowFindings[i]...)
			for w := range results {
				res.findings = append(res.findings, results[w].buckets[i]...)
			}
		}
	}
	for w := range results {
		res.skipped += results[w].rulesSkipped
		counter += results[w].counter
		if results[w].aborted {
			res.complete = false
		}
	}
	if opts.onProgress != nil && n > 0 {
		opts.onProgress(counter, total, enabled[n-1].Name())
	}
	return res
}

// visitUnsuppressed runs the enabled rules over ctx and hands every raw
// (unsuppressed, undeduplicated) finding to visit, the block-less parse-error
// fallback included, serialized and in no particular order: the suppression
// inventory's view of which rules fire where.
func visitUnsuppressed(ctx *RuleContext, rules []Rule, settings *models.AppSettings, visit func(f *models.Finding)) {
	var enabled []Rule
	for _, r := range rules {
		if RuleEnabled(r, settings) {
			enabled = append(enabled, r)
		}
	}
	parseErrorFired := false
	collectFindings(context.Background(), ctx, enabled, walkOptions{visit: func(f *models.Finding) {
		parseErrorFired = parseErrorFired || f.RuleID == "parse-error"
		visit(f)
	}})
	if parseErrorFired {
		return
	}
	fallback := parseErrorFallback(ctx.Flow, settings, nil)
	for i := range fallback {
		visit(&fallback[i])
	}
}

// sortByDocPosition stable-sorts a flow rule's findings into the document
// order of their anchor blocks; block-less findings and unknown anchors go
// last. Findings already in order stay as they are; the docPosition map is
// built only when that cannot be read off the eager index (inTopLevelOrder).
func sortByDocPosition(ctx *RuleContext, fs []models.Finding) {
	if len(fs) < 2 {
		return
	}
	same := true
	for i := 1; i < len(fs) && same; i++ {
		same = fs[i].BlockID == fs[0].BlockID
	}
	if same || inTopLevelOrder(ctx, fs) {
		return
	}
	pos := ctx.docPosition()
	key := func(f models.Finding) int {
		if p, ok := pos[f.BlockID]; ok && f.BlockID != "" {
			return p
		}
		return math.MaxInt
	}
	byPos := func(a, b models.Finding) int { return cmp.Compare(key(a), key(b)) }
	if !slices.IsSortedFunc(fs, byPos) {
		slices.SortStableFunc(fs, byPos)
	}
}

// inTopLevelOrder reports that fs is already in document order when every
// anchor is a top-level block of the subflow its SubflowID resolves to (where
// the built-in flow rules anchor) and the anchors ascend by (subflow, index),
// block-less findings last. False means "cannot tell": duplicate block IDs,
// a nested or foreign anchor, or findings out of order.
func inTopLevelOrder(ctx *RuleContext, fs []models.Finding) bool {
	if ctx.Flow == nil || len(ctx.AllBlocks) != ctx.totalBlocks {
		return false
	}
	subs := ctx.Flow.Subflows
	k, last, blockless := 0, -1, false
	for i := range fs {
		id := fs[i].BlockID
		if id == "" {
			blockless = true
			continue
		}
		b := ctx.AllBlocks[id]
		if blockless || b == nil || ctx.ParentMap[id] != "" {
			return false
		}
		sf := ctx.SubflowByID[b.SubflowID]
		if sf == nil {
			return false
		}
		// b is the j-th block of sf's own top-level group (a nested group
		// keyed by the same ID was ruled out above).
		j, group := ctx.BlockIndex[id], ctx.SiblingMap[sf.ID]
		if j >= len(group) || group[j] != b {
			return false
		}
		for k < len(subs) && &subs[k] != sf {
			k, last = k+1, -1
		}
		if k == len(subs) || j < last {
			return false
		}
		last = j
	}
	return true
}

// stampFindings fills ID/Fingerprint/Confidence/AutoFix/Stage on kept findings
// only; IDs number them in dedup order. Metadata is looked up by RuleID,
// except for findings already stamped stable.
func stampFindings(findings []models.Finding, keep []bool, flow *models.FlowDocument) {
	metas := metaTable()
	n := 0
	for i := range findings {
		if !keep[i] {
			continue
		}
		n++
		f := &findings[i]
		f.ID = fmt.Sprintf("F-%03d", n)
		// Fingerprint is content-derived (stable across re-imports/re-parses);
		// Key() stays for in-run uniqueness and legacy matching.
		f.Fingerprint = findingContentKey(*f, flow)
		m := metas[f.RuleID]
		if f.Stage == string(StageStable) {
			m = nil // a custom rule under a built-in's ID (collectFindings)
		}
		// Stamp per-rule confidence default for severity×confidence ordering.
		if f.Confidence == "" {
			f.Confidence = models.ConfidenceMedium
			if m != nil && m.Confidence != "" {
				f.Confidence = m.Confidence
			}
		}
		if m == nil {
			continue
		}
		if f.AutoFix == "" {
			f.AutoFix = m.AutoFix
		}
		if f.Stage == "" && m.Stage == StagePreview {
			f.Stage = string(StagePreview)
		}
	}
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
