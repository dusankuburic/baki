package service

import (
	"container/list"
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
	"weak"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/semaphore"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-core/logger"
	"pad-core/models"
)

// analysisReportBudget bounds the report pair cache by ESTIMATED BYTES (analyzer.ReportCost), not count.
const analysisReportBudget int64 = 96 << 20

// backgroundReportCeiling caps what a user-less caller may ADD to the pair cache — a sweep must not evict interactive entries.
const backgroundReportCeiling int64 = 16 << 20

// largeAnalysisBlocks is the block count from which an uncached analysis takes a concurrency slot.
const (
	largeAnalysisBlocks      = 20_000
	largeAnalysisConcurrency = 2
)

// largeAnalysisSem is the process-wide gate for large uncached analyses (shared by every instance).
var largeAnalysisSem = semaphore.NewWeighted(largeAnalysisConcurrency)

// analysisRunFunc is the analysis entry point called after the cache probe and gate; replaceable in tests.
type analysisRunFunc func(ctx context.Context, in analyzer.Input) *models.AnalysisReport

// reportPair tracks the two most recent distinct reports for a flow (for /api/analysis/diff).
type reportPair struct {
	prev    *models.AnalysisReport
	current *models.AnalysisReport
	// at is when current was last recorded (unix ms): the shared pair's run is served instead only when it is newer
	// (sharedAnalysis.run).
	at int64
}

// cost is the bytes the pair charges against analysisReportBudget.
func (p *reportPair) cost() int64 {
	return analyzer.ReportCost(p.current) + analyzer.ReportCost(p.prev)
}

// dropPrev forgets the pair's previous report.
func (p *reportPair) dropPrev() {
	p.prev = nil
}

// recordClaim is AnalyzeFlowFresh's dashboard-record claim; matched by weak pointer while alive, else content identity.
type recordClaim struct {
	report   weak.Pointer[models.AnalysisReport]
	content  string
	findings int
	stats    models.AnalysisStats
}

func newRecordClaim(r *models.AnalysisReport) recordClaim {
	return recordClaim{report: weak.Make(r), content: r.ContentHash, findings: len(r.Findings), stats: r.Stats}
}

// matches reports whether r is the report this claim was taken for.
func (c recordClaim) matches(r *models.AnalysisReport) bool {
	if c.report.Value() == r {
		return true
	}
	// Reports from non-cached paths carry no ContentHash; only the pointer identifies them.
	return c.content != "" && c.content == r.ContentHash && c.findings == len(r.Findings) && c.stats == r.Stats
}

// maxRecordClaims bounds recordClaims by count (a claim is ~100 B).
const maxRecordClaims = 4096

// recordClaims is the count-bounded LRU of dashboard-record claims, keyed by the flow's history key.
type recordClaims struct {
	mu    sync.Mutex
	max   int
	order *list.List // front = most recently used
	index map[string]*list.Element
}

type recordClaimEntry struct {
	key   string
	claim recordClaim
}

func newRecordClaims(max int) *recordClaims {
	return &recordClaims{max: max, order: list.New(), index: make(map[string]*list.Element)}
}

// take claims r for key and reports whether it was free, so per-report side effects run exactly once.
func (c *recordClaims) take(key string, r *models.AnalysisReport) (fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		e := el.Value.(*recordClaimEntry)
		if e.claim.matches(r) {
			c.order.MoveToFront(el)
			return false
		}
		c.order.Remove(el)
		delete(c.index, key)
	}
	for c.order.Len() >= c.max && c.order.Len() > 0 {
		back := c.order.Back()
		delete(c.index, back.Value.(*recordClaimEntry).key)
		c.order.Remove(back)
	}
	c.index[key] = c.order.PushFront(&recordClaimEntry{key: key, claim: newRecordClaim(r)})
	return true
}

// release gives the claim on key back if it names r; a no-op otherwise.
func (c *recordClaims) release(key string, r *models.AnalysisReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok && el.Value.(*recordClaimEntry).claim.matches(r) {
		c.order.Remove(el)
		delete(c.index, key)
	}
}

// reportLRU is the byte-weighted LRU behind AnalysisService.reports; get-mutate-put is serialised by the service.
type reportLRU struct {
	mu     sync.Mutex
	budget int64
	bytes  int64
	order  *list.List // front = most recently used
	index  map[string]*list.Element
}

type reportLRUEntry struct {
	key  string
	pair *reportPair
	cost int64
}

func newReportLRU(budget int64) *reportLRU {
	return &reportLRU{budget: budget, order: list.New(), index: make(map[string]*list.Element)}
}

// Get returns the pair stored under key and marks it most recently used.
func (c *reportLRU) Get(key string) (*reportPair, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*reportLRUEntry).pair, true
}

// Put stores pair under key, reporting retention: prev dropped if the pair exceeds the budget; LRU evicted by bytes.
func (c *reportLRU) Put(key string, pair *reportPair) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		c.evict(el)
	}
	if analyzer.ReportCost(pair.current) > c.budget {
		return false
	}
	if pair.cost() > c.budget {
		pair.dropPrev()
	}
	cost := pair.cost()
	for c.bytes+cost > c.budget && c.order.Len() > 0 {
		c.evict(c.order.Back())
	}
	el := c.order.PushFront(&reportLRUEntry{key: key, pair: pair, cost: cost})
	c.index[key] = el
	c.bytes += cost
	return true
}

// evict removes el and refunds its cost. Caller holds c.mu.
func (c *reportLRU) evict(el *list.Element) {
	e := el.Value.(*reportLRUEntry)
	c.order.Remove(el)
	delete(c.index, e.key)
	c.bytes -= e.cost
}

// Len is the number of retained pairs.
func (c *reportLRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Bytes is the estimated bytes charged by the retained pairs.
func (c *reportLRU) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// analysisHistoryKey returns a stable identity for trend/diff tracking (doc.ID is a fresh UUID per load).
func analysisHistoryKey(doc *models.FlowDocument) string {
	return analyzer.StableFlowID(doc)
}

// recordKey names a flow's report pair and dashboard-record claim, in this process and in Redis: a stored flow by its
// tenant and storage id, anything else (a desktop document, a path) by its stable identity under "path". A
// stored folder re-parsed by a cloud fix or a snapshot restore carries the parser's StableID, a hash of its file
// names, which another organisation's folder of the same names shares: keyed by it, one tenant's report was served
// for the other's flow.
type recordKey struct {
	tenant, flow string
	// part is the tenant as a cache key part (sharedRuns).
	part cache.Part
}

func analysisRecordKey(doc *models.FlowDocument) recordKey {
	if doc.FilePath == "" && doc.ID != "" {
		switch {
		case doc.OrganizationID != "":
			return recordKey{"org-" + doc.OrganizationID, doc.ID, cache.Org(doc.OrganizationID)}
		case doc.OwnerID != "":
			return recordKey{"user-" + doc.OwnerID, doc.ID, cache.User(doc.OwnerID)}
		}
	}
	return recordKey{"path", analysisHistoryKey(doc), cache.Local()}
}

// String is the key of the in-process pair and claim maps.
func (k recordKey) String() string { return k.tenant + "/" + k.flow }

type AnalysisService struct {
	notifier    EventNotifier
	settings    SettingsProvider
	history     *analyzer.HistoryStore
	customRules []analyzer.Rule
	// profiles resolves the per-org rule profile; nil (desktop/tests) falls back to deployment settings + custom rules.
	profiles *RuleProfileResolver
	// authEnabled: with accounts, a claim-less analysis has no one to send progress to (EmitTo(LocalUser) reaches
	// only identity-less clients, which exist only in desktop mode).
	authEnabled bool
	// runsLibrary: the deployment runs the library rules (AnalysisOptions.LibraryRules), so the catalog lists them.
	runsLibrary bool
	// bus announces the rule toggles (deployment SettingsChanged); nil publishes nothing.
	bus *eventbus.Bus
	// analyzer caches reports and inventories; the process's one, shared with FlowService, which invalidates it.
	analyzer *analyzer.CachedAnalyzer

	// largeSem gates uncached analyses of large flows (nil disables); run is the entry point after the probe
	// (s.analyzer.Analyze unless a test replaces it).
	largeSem *semaphore.Weighted
	run      analysisRunFunc

	mu      sync.Mutex
	reports *reportLRU
	claims  *recordClaims
	// shared is the other replicas' view of the pair and the claims (AnalysisOptions.Shared); nil: none.
	shared *sharedAnalysis
}

// AnalysisOptions are AnalysisService's deployment wiring; the zero value analyzes on deployment settings with
// the built-in rules and broadcasts claim-less progress (the desktop).
type AnalysisOptions struct {
	// CustomRules are the compiled deployment rules (PAD_CUSTOM_RULES); DI hands the same slice to the
	// RuleProfileResolver.
	CustomRules DeploymentCustomRules
	// Profiles resolves the per-org rule profile; nil falls back to deployment settings + custom rules.
	Profiles *RuleProfileResolver
	// AuthEnabled: requests carry auth, so claim-less progress is dropped (not broadcast).
	AuthEnabled bool
	// LibraryRules: the deployment runs the library rules (desktop folder analysis, AnalyzeBatch: no accounts), so
	// the rule catalog lists them; elsewhere they never run and stay out of it.
	LibraryRules bool
	// Bus announces the deployment rule toggles (SettingsChanged); nil publishes nothing.
	Bus *eventbus.Bus
	// Analyzer is the process's cached analyzer (internal/di), shared with FlowService so a reimport, a deleted
	// flow and a desktop folder open reach the reports this service serves; nil builds a private one.
	Analyzer *analyzer.CachedAnalyzer
	// Shared is the deployment's Redis when it can run more than one replica (PAD_MAX_REPLICAS > 1): the report
	// pair and the dashboard-record claims are then the deployment's (analysis_shared.go); nil keeps them per
	// process.
	Shared *redis.Client
}

// NewAnalysisService builds the service; the error return is kept for the DI signature.
func NewAnalysisService(notifier EventNotifier, settings SettingsProvider, history *analyzer.HistoryStore, opts AnalysisOptions) (*AnalysisService, error) {
	a := opts.Analyzer
	if a == nil {
		a = NewDefaultAnalyzer()
	}
	return &AnalysisService{
		notifier:    notifier,
		settings:    settings,
		history:     history,
		customRules: opts.CustomRules,
		profiles:    opts.Profiles,
		authEnabled: opts.AuthEnabled,
		runsLibrary: opts.LibraryRules,
		bus:         opts.Bus,
		analyzer:    a,
		largeSem:    largeAnalysisSem,
		reports:     newReportLRU(analysisReportBudget),
		claims:      newRecordClaims(maxRecordClaims),
		shared:      newSharedAnalysis(opts.Shared),
	}, nil
}

// NewDefaultAnalyzer is a cached analyzer with the default bounds: 50 reports within analyzer.DefaultCacheBudget,
// 64 suppression inventories.
func NewDefaultAnalyzer() *analyzer.CachedAnalyzer {
	return analyzer.NewCachedAnalyzer(
		analyzer.WithReportCache(analyzer.DefaultReportCacheEntries, analyzer.DefaultCacheBudget),
		analyzer.WithInventoryCache(analyzer.DefaultInventoryCacheEntries))
}

// cacheTenant is whose report doc's is, for the analyzer's cache. A report depends on the flow's content and its
// owning org's rule profile, not on who asks, so a stored flow has one entry for everyone who may read it. With
// accounts off (the desktop, local mode) everything is the local user's; another path-less input is the caller's
// (AnalyzeRaw skips the cache with accounts); without either there is nobody to scope to, and the zero part analyzes
// without caching.
func (s *AnalysisService) cacheTenant(ctx context.Context, doc *models.FlowDocument) cache.Part {
	if !s.authEnabled {
		return cache.Local()
	}
	if t := cache.Tenant(doc.OrganizationID, doc.OwnerID); !t.IsZero() {
		return t // a stored flow: resolveStoredFlow sets both
	}
	if c := auth.ClaimsFromContext(ctx); c != nil && c.UserID != "" {
		return cache.User(c.UserID)
	}
	return cache.Part{}
}

// AuthEnabled reports AnalysisOptions.AuthEnabled.
func (s *AnalysisService) AuthEnabled() bool {
	return s.authEnabled
}

func (s *AnalysisService) AnalyzeFlow(ctx context.Context, doc *models.FlowDocument) (report *models.AnalysisReport, err error) {
	report, _, err = s.analyzeFlow(ctx, doc, analyzeOpts{record: true, progress: true})
	return report, err
}

// AnalyzeFlowFresh is AnalyzeFlow that also CLAIMS the dashboard record: fresh exactly once per report.
func (s *AnalysisService) AnalyzeFlowFresh(ctx context.Context, doc *models.FlowDocument) (report *models.AnalysisReport, fresh bool, err error) {
	return s.analyzeFlow(ctx, doc, analyzeOpts{record: true, progress: true, claim: true})
}

// ReleaseRecord gives back the dashboard-record claim for report; a no-op once the claim moved to a newer report.
func (s *AnalysisService) ReleaseRecord(doc *models.FlowDocument, report *models.AnalysisReport) {
	if s == nil || s.claims == nil || report == nil {
		return
	}
	key := analysisRecordKey(doc)
	s.claims.release(key.String(), report)
	s.shared.release(context.Background(), key, report)
}

// AnalyzeFlowBackground analyzes and RECORDS but emits no progress; above backgroundReportCeiling nothing is ADDED.
func (s *AnalysisService) AnalyzeFlowBackground(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
	report, _, err := s.analyzeFlow(ctx, doc, analyzeOpts{record: true, background: true})
	return report, err
}

// AnalyzeRaw analyzes a path-less input (POST /api/analysis/analyze-raw) with progress but WITHOUT recording: it is
// no stored flow, so it has no diff pair or history to update. It still counts as an analysis run. With accounts it
// bypasses the report cache: each post is a fresh parse that no later request hits, and only the desktop session
// analytics (no accounts) read entries back.
func (s *AnalysisService) AnalyzeRaw(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
	report, _, err := s.analyzeFlow(ctx, doc, analyzeOpts{count: true, progress: true, uncached: s.authEnabled})
	return report, err
}

// AnalyzeFlowReadOnly analyzes WITHOUT recording or progress, so internal analyses cannot overwrite the diff pair.
func (s *AnalysisService) AnalyzeFlowReadOnly(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
	report, _, err := s.analyzeFlow(ctx, doc, analyzeOpts{})
	return report, err
}

// analyzeOpts selects what an analyzeFlow call records and emits (claim and background imply record, record implies
// count).
type analyzeOpts struct {
	record     bool
	count      bool // an analysis run for pad_analysis_runs_total
	progress   bool
	claim      bool
	background bool
	uncached   bool // the zero tenant: neither served from nor stored in the report cache
}

// analyzeFlow runs the cached analysis under opts; fresh reports whether a claim was still free.
func (s *AnalysisService) analyzeFlow(ctx context.Context, doc *models.FlowDocument, opts analyzeOpts) (report *models.AnalysisReport, fresh bool, err error) {
	defer logger.Guard("App.AnalyzeFlow", &err)

	record, progress, claim := opts.record, opts.progress, opts.claim

	if record || opts.count {
		recordAnalysisRun()
	}

	// Trace the run so its duration is attributable within the request trace; no-op without an OTLP exporter.
	_, span := otel.Tracer("analysis-service").Start(ctx, "AnalyzeFlow")
	defer span.End()

	if doc == nil {
		err = fmt.Errorf("no flow loaded")
		span.RecordError(err)
		return nil, false, err
	}

	// Rules run per the flow's OWNING ORG (pre-R4: process-global, one tenant's toggle changed every tenant).
	profile := s.profileFor(ctx, doc)
	settings, rules := profile.Settings, profile.Rules
	// A non-member's view lacks the org's custom rules: the run is theirs alone, cached apart from the org's
	// report (whose entry it would replace) and recorded nowhere members read (the diff pair, exports, chat,
	// history, the dashboard).
	if profile.Narrowed {
		record, claim = false, false
	}

	// Extract userID for per-user event delivery (prevents cross-tenant leaks).
	userID := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
	}

	// No callback when nobody is listening: with auth on, a claim-less context has no recipient.
	var onProgress func(current, total int, ruleName string)
	if progress && (userID != "" || !s.authEnabled) {
		onProgress = func(current, total int, ruleName string) {
			s.notifier.EmitTo(userID, "analysis:progress", map[string]any{
				"current":  current,
				"total":    total,
				"ruleName": ruleName,
			})
		}
	}
	in := analyzer.Input{Doc: doc, Rules: rules, Settings: settings, OnProgress: onProgress}
	if !opts.uncached {
		in.Tenant = s.cacheTenant(ctx, doc)
		if profile.Narrowed {
			in.Tenant = narrowedTenant(ctx, doc)
		}
	}
	// Probe the cache first so a hit never queues behind the large-analysis gate.
	result := s.analyzer.Lookup(in)
	if result == nil {
		result, err = s.runGated(ctx, in)
		if err != nil {
			span.RecordError(err)
			return nil, false, err
		}
	}
	// nil with ctx cancelled: the truncated report is discarded inside the analyzer — surface the cancellation.
	if result == nil && ctx.Err() != nil {
		err = ctx.Err()
		span.RecordError(err)
		return nil, false, err
	}
	// Any other nil would be an analyzer bug; fail gracefully instead of panicking downstream.
	if result == nil {
		err = fmt.Errorf("analysis produced no result for flow %q", doc.ID)
		span.RecordError(err)
		return nil, false, err
	}

	// Surface skipped rules (panic recovery) so ops can alert when a rule silently produces no findings.
	if result.Stats.RulesSkipped > 0 {
		recordRulesSkipped(result.Stats.RulesSkipped)
	}

	span.SetAttributes(
		attribute.String("flow.id", result.FlowID),
		attribute.Int("analysis.findings", len(result.Findings)),
	)

	// Track the two most recent distinct runs for diffing.
	if record {
		rk := analysisRecordKey(doc)
		key := rk.String()
		s.mu.Lock()
		pair, ok := s.reports.Get(key)
		if !ok {
			pair = &reportPair{}
		}
		newRun := false
		switch {
		case pair.current == result: // a cache hit
		case sameRun(pair.current, result):
			// The same content and findings from a fresh parse (a desktop folder reload mints new ids): the
			// current report takes the new ids, and the previous distinct run stays for the diff.
			pair.current = result
		default:
			newRun = true
			pair.prev = pair.current
			pair.current = result
		}
		at := time.Now().UnixMilli()
		pair.at = at
		// The claim is separate from the run tracking and lives outside the budgeted pair cache.
		if claim {
			fresh = s.claims.take(key, result)
		}
		// Re-put after mutating (the pair's cost changed); a background caller may not add a large one.
		retain := ok || !opts.background || analyzer.ReportCost(result) <= backgroundReportCeiling
		if retain {
			s.reports.Put(key, pair)
		}
		s.mu.Unlock()
		// The other replicas' view: the run, its report unless a sweep made it, and the claim, which the shared one
		// decides while Redis answers.
		if s.shared != nil {
			if retain {
				s.shared.record(ctx, rk, result, at, !opts.background)
			}
			if claim {
				if f, ok := s.shared.claim(ctx, rk, result); ok {
					fresh = f
				}
			}
		}
		if newRun && s.history != nil {
			s.history.Record(analysisHistoryKey(doc), result, doc)
		}
	}

	// NOTE: no "analysis:complete" SSE broadcast — no client consumes it and it shipped megabytes.

	logger.Info("analysis complete",
		"flowId", result.FlowID,
		"findings", len(result.Findings),
		"durationMs", result.DurationMs,
	)

	return result, fresh, nil
}

// ReadsOrgView reports whether ctx's reader analyzes doc under its organisation's own rules: false for a
// non-member reading under row-level security (RuleProfile.Narrowed), whose run must not become the flow's shared
// state (a baseline, for one).
func (s *AnalysisService) ReadsOrgView(ctx context.Context, doc *models.FlowDocument) bool {
	return doc == nil || !s.profileFor(ctx, doc).Narrowed
}

// narrowedTenant is the report cache tenant of a narrowed run (RuleProfile.Narrowed): the reader's own within the
// org, so it neither serves nor replaces the org's entry for the flow; zero (uncached) without a calling user.
func narrowedTenant(ctx context.Context, doc *models.FlowDocument) cache.Part {
	a := auth.ActorFromContext(ctx)
	if a == nil || a.UserID == "" || doc.OrganizationID == "" {
		return cache.Part{}
	}
	return cache.OrgUser(doc.OrganizationID, a.UserID)
}

// sameRun reports whether b repeats a: the same content (ContentHash; reports of uncached paths carry none, and
// only their pointer identifies them) and the same findings in the same order. Ids aside, a re-parse of unchanged
// text under an unchanged profile is not a new run.
func sameRun(a, b *models.AnalysisReport) bool {
	if a == nil || b == nil || a.ContentHash == "" || a.ContentHash != b.ContentHash || len(a.Findings) != len(b.Findings) {
		return false
	}
	for i := range a.Findings {
		fa, fb := &a.Findings[i], &b.Findings[i]
		if fa.RuleID != fb.RuleID || fa.Fingerprint != fb.Fingerprint || fa.Severity != fb.Severity || fa.Stage != fb.Stage {
			return false
		}
	}
	return a.Stats == b.Stats
}

// runGated runs the analysis holding a large-analysis slot when large; released on every exit.
func (s *AnalysisService) runGated(ctx context.Context, in analyzer.Input) (*models.AnalysisReport, error) {
	release, err := s.acquireLargeSlot(ctx, in.Doc)
	if err != nil {
		return nil, err
	}
	defer release()
	run := s.run
	if run == nil {
		run = s.analyzer.Analyze
	}
	return run(ctx, in), nil
}

// acquireLargeSlot takes a largeAnalysisSem slot for a large uncached flow; small flows get a no-op release.
func (s *AnalysisService) acquireLargeSlot(ctx context.Context, doc *models.FlowDocument) (release func(), err error) {
	if s.largeSem == nil || countBlocks(doc) < largeAnalysisBlocks {
		return func() {}, nil
	}
	if !s.largeSem.TryAcquire(1) {
		logger.Info("analysis waiting for a large-analysis slot", "flowId", doc.ID, "limit", largeAnalysisConcurrency)
		if err := s.largeSem.Acquire(ctx, 1); err != nil {
			return nil, err
		}
	}
	return func() { s.largeSem.Release(1) }, nil
}

// countBlocks is the block count, children included — a plain walk that relies on no transient index.
func countBlocks(doc *models.FlowDocument) int {
	n := 0
	for i := range doc.Subflows {
		n += countBlockTree(doc.Subflows[i].Blocks)
	}
	return n
}

func countBlockTree(blocks []models.Block) int {
	n := len(blocks)
	for i := range blocks {
		if len(blocks[i].Children) > 0 {
			n += countBlockTree(blocks[i].Children)
		}
	}
	return n
}

// TestRule runs ONE candidate rule and returns its findings — nothing cached, recorded or emitted. The rule is
// checked strictly, as a save would, except for a built-in rule's id: a test stores nothing, and a rule saved under
// one before ids were checked can still be edited (OrgRuleService.SaveRule), so its editor must be able to test it.
// A rule it refuses is the *analyzer.CustomRuleError.
func (s *AnalysisService) TestRule(ctx context.Context, doc *models.FlowDocument, cfg analyzer.CustomRuleConfig) ([]models.Finding, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	for _, p := range analyzer.CheckCustomRule(cfg, analyzer.CustomRuleStrict) {
		if !p.Warning && p.Reason != analyzer.ReasonReservedID {
			return nil, fmt.Errorf("invalid rule: %w", p)
		}
	}
	// Tolerant differs from Strict only in what the loop above already refused, and in the built-in id.
	rule, _, err := analyzer.CompileCustomRule(cfg, analyzer.CustomRuleTolerant)
	if err != nil {
		return nil, fmt.Errorf("invalid rule: %w", err)
	}
	// Settings are nil: the org profile would let a DISABLED same-id rule silence the rule being tested.
	report := analyzer.RunAnalysisCtx(ctx, doc, []analyzer.Rule{rule}, nil, nil)
	// A cancelled walk skips the remaining blocks: its findings are a truncated subset.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if report == nil {
		return nil, fmt.Errorf("analysis produced no result")
	}
	return report.Findings, nil
}

// PreviousReport returns the analysis run recorded immediately before the current one.
func (s *AnalysisService) PreviousReport(doc *models.FlowDocument) (*models.AnalysisReport, bool) {
	return s.recordedRun(doc, true)
}

// CurrentReport returns the flow's most recent report; nil-safe on an unconfigured service.
func (s *AnalysisService) CurrentReport(doc *models.FlowDocument) (*models.AnalysisReport, bool) {
	return s.recordedRun(doc, false)
}

// recordedRun is the pair's previous or current run: the deployment's when the pair is shared and Redis answers
// (this replica's own report when it holds that run), else this replica's.
func (s *AnalysisService) recordedRun(doc *models.FlowDocument, previous bool) (*models.AnalysisReport, bool) {
	if s == nil || s.reports == nil {
		return nil, false
	}
	key := analysisRecordKey(doc)
	var cur, prev *models.AnalysisReport
	var at int64
	s.mu.Lock()
	if pair, _ := s.reports.Get(key.String()); pair != nil {
		cur, prev, at = pair.current, pair.prev, pair.at
	}
	s.mu.Unlock()
	if r, ok := s.shared.run(key, previous, cur, prev, at); ok {
		return r, r != nil
	}
	if previous {
		return prev, prev != nil
	}
	return cur, cur != nil
}

// History returns the persisted analysis trend snapshots for a flow.
func (s *AnalysisService) History(doc *models.FlowDocument) []analyzer.AnalysisSnapshot {
	if s.history == nil {
		return nil
	}
	return s.history.Load(analysisHistoryKey(doc))
}

func (s *AnalysisService) GetVariableLineage(doc *models.FlowDocument, varName string) (history *models.VariableHistory, err error) {
	defer logger.Guard("App.GetVariableLineage", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	return analyzer.BuildVariableLineage(doc, varName), nil
}

func (s *AnalysisService) GetExecutionGraph(doc *models.FlowDocument) (graph *models.GraphData, err error) {
	defer logger.Guard("App.GetExecutionGraph", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	return analyzer.BuildExecutionGraph(doc, nil), nil
}

func (s *AnalysisService) GetDataFlow(doc *models.FlowDocument) (df *models.DataFlowAnalysis, err error) {
	defer logger.Guard("App.GetDataFlow", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	return analyzer.AnalyzeDataFlow(doc), nil
}

func (s *AnalysisService) AnalyzeBatch(docs []*models.FlowDocument) (batch *models.BatchAnalysis, err error) {
	defer logger.Guard("App.AnalyzeBatch", &err)

	if len(docs) == 0 {
		return nil, fmt.Errorf("no flows provided")
	}

	settings := s.settings.Get()
	rules := analyzer.AllRules()
	if len(s.customRules) > 0 {
		rules = append(rules, s.customRules...)
	}
	// Desktop only (the route is refused with accounts): the reports join the local session analytics.
	batch = s.analyzer.Batch(docs, analyzer.Input{Rules: rules, Settings: settings, Tenant: cache.Local()})
	// The library rules (preview) compare the folder's flows with each other: one pass over every document, after
	// the per-flow analyses. Their findings stay in batch.Library (nil when no library rule is enabled), never in
	// a flow's report, its cache entry or a gate.
	batch.Library = analyzer.AnalyzeLibrary(context.Background(), docs, analyzer.LibraryRules(), settings)
	return batch, nil
}

func (s *AnalysisService) DiffReports(old, new *models.AnalysisReport) *models.AnalysisDiff {
	return analyzer.DiffReports(old, new)
}

func (s *AnalysisService) GetDependencyAnalysis() *models.DependencyAnalysis {
	return analyzer.AnalyzeRuleDependencies()
}

// ComputeDashboard aggregates the desktop session analytics: the local user's cached reports, one per flow.
func (s *AnalysisService) ComputeDashboard() *models.DashboardStats {
	return analyzer.ComputeDashboard(sortedReports(s.sessionReports()))
}

// DashboardData returns stats and reports from one cache snapshot, so all sections see a consistent view.
func (s *AnalysisService) DashboardData() (*models.DashboardStats, []*models.AnalysisReport) {
	reports := sortedReports(s.sessionReports())
	return analyzer.ComputeDashboard(reports), reports
}

// sessionReports is the local user's cached reports; another tenant's never reach the session analytics.
func (s *AnalysisService) sessionReports() []*models.AnalysisReport {
	if s == nil {
		return nil
	}
	return s.analyzer.Reports(cache.Local())
}

// sortedReports orders a cache snapshot by FlowID (the cache iterates a map).
func sortedReports(reports []*models.AnalysisReport) []*models.AnalysisReport {
	sort.Slice(reports, func(i, j int) bool { return reports[i].FlowID < reports[j].FlowID })
	return reports
}

func (s *AnalysisService) DeduplicateFindings(report *models.AnalysisReport) ([]models.Finding, []models.FindingGroup) {
	return analyzer.DeduplicateFindings(report.Findings)
}

func (s *AnalysisService) FindRelatedFindings(report *models.AnalysisReport, blockID string) []models.Finding {
	return analyzer.FindRelatedFindings(report.Findings, blockID)
}

func (s *AnalysisService) CompareFlows(docA, docB *models.FlowDocument) *models.FlowComparison {
	return analyzer.CompareFlows(docA, docB)
}

// GetRules returns the deployment-scoped rule catalog; prefer GetRulesFor in handlers so org members see the rules that actually run.
func (s *AnalysisService) GetRules() []models.Rule {
	return s.GetRulesFor(context.Background(), "")
}

// GetRulesFor returns the catalog as it applies to orgID — through the same resolver the analysis uses — plus the
// library rules where the deployment runs them (AnalysisOptions.LibraryRules).
func (s *AnalysisService) GetRulesFor(ctx context.Context, orgID string) (result []models.Rule) {
	defer logger.GuardRecover("App.GetRules")

	var settings *models.AppSettings
	var rules []analyzer.Rule
	if orgID != "" && s.profiles != nil {
		profile := s.profiles.Resolve(ctx, orgID)
		settings, rules = profile.Settings, profile.Rules
	} else {
		if s.settings != nil {
			settings = s.settings.Get()
		}
		rules = analyzer.AllRules()
		if len(s.customRules) > 0 {
			rules = append(rules, s.customRules...)
		}
	}
	if s.runsLibrary {
		// A new slice: a profile's rule slice is the resolver's and must not be appended to in place.
		lib := analyzer.LibraryRules()
		all := make([]analyzer.Rule, 0, len(rules)+len(lib))
		all = append(all, rules...)
		for _, r := range lib {
			all = append(all, r)
		}
		rules = all
	}
	defaults := analyzer.DefaultRuleConfigs()
	result = make([]models.Rule, len(rules))
	for i, r := range rules {
		result[i] = catalogRule(r, settings, defaults)
	}
	return result
}

// catalogRule is r's catalog entry under settings: enabled and metadata as the engine applies them
// (RuleEnabled, MetaOf), the configured severity and options, else the built-in option defaults.
func catalogRule(r analyzer.Rule, settings *models.AppSettings, defaults map[string]models.RuleConfig) models.Rule {
	m := analyzer.MetaOf(r)
	out := models.Rule{
		ID:              r.ID(),
		Name:            r.Name(),
		Description:     r.Description(),
		DefaultSeverity: r.DefaultSeverity(),
		Category:        r.Category(),
		Enabled:         analyzer.RuleEnabled(r, settings),
		Confidence:      m.Confidence,
		AutoFix:         m.AutoFix,
		Stage:           string(m.Stage),
		IntroducedIn:    m.IntroducedIn,
		Kind:            analyzer.RuleKind(r),
	}
	if out.Confidence == "" {
		out.Confidence = models.ConfidenceMedium
	}
	// A fix the rule stamps itself (subflow-mismatch's append-output, a custom rule's autoFix) is one-click too.
	if out.AutoFix == "" && len(m.DirectFixes) > 0 {
		out.AutoFix = m.DirectFixes[0]
	}
	if settings != nil {
		if rc, ok := settings.Analysis.Rules[r.ID()]; ok {
			// Surface the configured severity override, else a later config save silently reverts it; as the engine
			// applies it (a stored "critical" runs as error, and the config route refuses it on the way back).
			if sev := analyzer.NormalizeSeverity(rc.Severity); sev != "" {
				out.DefaultSeverity = sev
			}
			if len(rc.Options) > 0 {
				out.Options = rc.Options
			}
		}
	}
	// Surface built-in threshold defaults for rules that carry them (reachable only by hand-editing settings JSON).
	if out.Options == nil {
		if d, ok := defaults[r.ID()]; ok && len(d.Options) > 0 {
			out.Options = d.Options
		}
	}
	return out
}

// GetRulesSummary folds the catalog into the dashboard's counts, reusing GetRules (same overrides).
func (s *AnalysisService) GetRulesSummary() models.RuleSummary {
	return s.GetRulesSummaryFor(context.Background(), "")
}

// GetRulesSummaryFor is GetRulesSummary over the org-scoped catalog.
func (s *AnalysisService) GetRulesSummaryFor(ctx context.Context, orgID string) models.RuleSummary {
	defer logger.GuardRecover("App.GetRulesSummary")

	summary := models.RuleSummary{
		ByCategory:   map[string]int{},
		ByConfidence: map[string]int{},
	}
	for _, r := range s.GetRulesFor(ctx, orgID) {
		summary.TotalRules++
		if r.AutoFix != "" {
			summary.AutoFixableRules++
		}
		if r.Category != "" {
			summary.ByCategory[r.Category]++
		}
		c := string(r.Confidence)
		if c == "" {
			c = string(models.ConfidenceMedium)
		}
		summary.ByConfidence[c]++
	}
	return summary
}

// SetRuleEnabled writes the DEPLOYMENT-WIDE settings singleton; in cloud mode the write joins the request's RLS transaction.
func (s *AnalysisService) SetRuleEnabled(ctx context.Context, ruleID string, enabled bool) (err error) {
	defer logger.Guard("App.SetRuleEnabled", &err)

	return s.updateDeploymentRules(ctx, func(rules map[string]models.RuleConfig) {
		rc := rules[ruleID]
		rc.Enabled = enabled
		rules[ruleID] = rc
	})
}

// updateDeploymentRules changes the deployment document's rules (only them: the rest is written back as stored,
// never from this replica's cache, so a rule write cannot revert an admin's AI settings) and announces it: every
// org's rule profile inherits them.
func (s *AnalysisService) updateDeploymentRules(ctx context.Context, change func(rules map[string]models.RuleConfig)) error {
	err := updateSettingsWith(ctx, s.settings, func(settings *models.AppSettings) error {
		if settings.Analysis.Rules == nil {
			settings.Analysis.Rules = make(map[string]models.RuleConfig)
		}
		change(settings.Analysis.Rules)
		return nil
	})
	if err != nil {
		return err
	}
	s.bus.Publish(ctx, events.SettingsChanged{Scope: events.DeploymentSettings})
	return nil
}

// UpdateRuleConfig writes the deployment-wide settings singleton; see SetRuleEnabled for the context.
func (s *AnalysisService) UpdateRuleConfig(ctx context.Context, ruleID string, config models.RuleConfig) (err error) {
	defer logger.Guard("App.UpdateRuleConfig", &err)

	return s.updateDeploymentRules(ctx, func(rules map[string]models.RuleConfig) {
		// Merge semantics: clients sending only {enabled, severity} must not wipe configured thresholds (e.g. maxDepth).
		if config.Options == nil {
			config.Options = rules[ruleID].Options
		}
		rules[ruleID] = config
	})
}
