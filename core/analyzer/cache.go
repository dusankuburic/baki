package analyzer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"weak"

	"golang.org/x/sync/singleflight"

	"pad-core/cache"
	"pad-core/models"
)

// The analyzer's caches (ADR 0017). A report and a suppression inventory are functions of the document (its
// content and the identifiers its parse minted), the rule profile and the analyzer version; the tenant part keeps
// one tenant's entries, and the desktop session analytics read from them, apart from every other tenant's.
var (
	reportSpec = cache.Declare("analyzer.reports",
		cache.Keys(cache.KindTenant, cache.KindResource, cache.KindVersion, cache.KindRuleProfile, cache.KindContent),
		cache.ReplicaLocal("a function of the document, rule profile and analyzer version; another replica recomputes on a miss, and the desktop session analytics it backs live in one process"))
	inventorySpec = cache.Declare("analyzer.suppression-inventory",
		cache.Keys(cache.KindTenant, cache.KindVersion, cache.KindRuleProfile, cache.KindContent),
		cache.ReplicaLocal("a function of the document, rule profile and analyzer version; another replica recomputes on a miss"))
)

// analyzerVersion is the version part of the analyzer's cache keys (not of FlowHash, the pure content identity);
// bump it when rule logic or parser output changes.
const analyzerVersion = "6"

var analyzerVersionPart = cache.Version(analyzerVersion)

// Defaults of the process's analyzer (internal/di): reports by count and by estimated bytes, inventories by count.
const (
	DefaultReportCacheEntries    = 50
	DefaultInventoryCacheEntries = 64
)

// DefaultCacheBudget is the default byte budget of the report cache: ~2x the measured per-finding size, enough for
// hundreds of typical reports or one finding-dense flow in a 1 GiB container.
const DefaultCacheBudget int64 = 96 << 20

// Report cost model, shared with the service-layer report cache: flat base
// plus a per-finding charge ~2x the measured retained size, erring safe.
const (
	reportCostBase       int64 = 4 << 10
	reportCostPerFinding int64 = 1 << 10
)

// ReportCost estimates the bytes a report keeps resident; nil costs nothing.
func ReportCost(r *models.AnalysisReport) int64 {
	if r == nil {
		return 0
	}
	return reportCostBase + reportCostPerFinding*int64(len(r.Findings))
}

// Input is one analysis request. Tenant is whose report it is (cache.Local on the desktop, the stored flow's owner,
// or the caller): the cache keeps tenants apart. A zero Tenant, or a document with no identity (no FilePath,
// StableID or ID), is analyzed without the cache: never stored, never joined with another caller's walk.
type Input struct {
	Doc        *models.FlowDocument
	Rules      []Rule
	Settings   *models.AppSettings
	Tenant     cache.Part
	OnProgress func(current, total int, rule string)
}

// CachedAnalyzer analyzes documents behind the report and suppression-inventory caches. The process has one
// (internal/di), shared by every service that analyzes or invalidates. A nil or option-less CachedAnalyzer
// analyzes without caching.
type CachedAnalyzer struct {
	reports   *reportCache
	inventory *cache.Typed[[]SuppressionEntry]
	// flights runs one walk per full report key at a time: callers the key separates never share one.
	flights singleflight.Group
}

// CacheOption configures a CachedAnalyzer.
type CacheOption func(*CachedAnalyzer)

// WithReportCache caches reports, at most maxEntries within maxBytes of ReportCost (non-positive values select
// DefaultReportCacheEntries and DefaultCacheBudget). A report costlier than the budget is served but not kept.
func WithReportCache(maxEntries int, maxBytes int64) CacheOption {
	return func(a *CachedAnalyzer) { a.reports = newReportCache(maxEntries, maxBytes) }
}

// WithInventoryCache caches suppression inventories, at most maxEntries (non-positive: DefaultInventoryCacheEntries).
func WithInventoryCache(maxEntries int) CacheOption {
	return func(a *CachedAnalyzer) {
		if maxEntries <= 0 {
			maxEntries = DefaultInventoryCacheEntries
		}
		a.inventory = cache.New[[]SuppressionEntry](inventorySpec, int64(maxEntries))
	}
}

// NewCachedAnalyzer builds an analyzer with the given caches; without options it caches nothing.
func NewCachedAnalyzer(opts ...CacheOption) *CachedAnalyzer {
	a := &CachedAnalyzer{}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Analyze returns in.Doc's report: the cached one, else the result of one walk shared by the concurrent callers
// of the same key. It returns nil only when ctx ended before the walk completed: a truncated report is neither
// cached nor handed out, and a caller that joined a walk whose leader left walks again under its own context.
func (a *CachedAnalyzer) Analyze(ctx context.Context, in Input) *models.AnalysisReport {
	if ctx == nil {
		ctx = context.Background()
	}
	if in.Doc == nil {
		return nil
	}
	k, ok := a.reportKey(in)
	if !ok {
		return walkReport(ctx, in)
	}
	if r := a.reports.get(k); r != nil {
		reportSpec.RecordHit()
		return r
	}
	reportSpec.RecordMiss()
	walk := func() (*models.AnalysisReport, error) {
		r := walkReport(ctx, in)
		if r == nil {
			return nil, errAnalysisIncomplete
		}
		a.reports.put(k, r)
		return r, nil
	}
	// Joiners run under the LEADER's context: a cancelled leader fails the flight with errAnalysisIncomplete, and a
	// live joiner starts a fresh flight rather than receiving a truncated report.
	for range maxAnalysisFlightAttempts {
		v, err, _ := a.flights.Do(k.key.String(), func() (any, error) {
			// A previous leader may have stored the report between our miss and this flight.
			if r := a.reports.get(k); r != nil {
				return r, nil
			}
			return walk()
		})
		if err == nil {
			return v.(*models.AnalysisReport)
		}
		if ctx.Err() != nil {
			return nil
		}
		if !errors.Is(err, errAnalysisIncomplete) {
			break
		}
	}
	// Repeatedly joined cancelled leaders: walk directly under our own context.
	r, _ := walk()
	return r
}

// Lookup returns the report the cache already holds for in without analyzing, so a hit never waits for a walk or a
// large-analysis slot; nil on a miss. It counts hits only: the Analyze that follows a miss counts it.
func (a *CachedAnalyzer) Lookup(in Input) *models.AnalysisReport {
	if in.Doc == nil {
		return nil
	}
	k, ok := a.reportKey(in)
	if !ok {
		return nil
	}
	r := a.reports.get(k)
	if r != nil {
		reportSpec.RecordHit()
	}
	return r
}

// SuppressionInventory is the package's SuppressionInventory behind the inventory cache, one build per key at a
// time. The slice is shared with every caller of the key: read-only.
func (a *CachedAnalyzer) SuppressionInventory(in Input) []SuppressionEntry {
	if in.Doc == nil || !hasPadIgnoreDirective(in.Doc) {
		return nil
	}
	build := func(context.Context) ([]SuppressionEntry, int64, error) {
		return SuppressionInventory(in.Doc, in.Rules, in.Settings), 1, nil
	}
	if a == nil || a.inventory == nil || in.Tenant.IsZero() {
		entries, _, _ := build(context.Background())
		return entries
	}
	// A key the spec refuses is the zero Key, which GetOrFill builds without the cache.
	k, _ := inventorySpec.Key(in.Tenant, analyzerVersionPart, cache.RuleProfile(ProfileDigest(in.Settings, in.Rules)), contentPart(in.Doc))
	entries, _ := a.inventory.GetOrFill(context.Background(), k, build, nil, "")
	return entries
}

// InvalidateFlow drops every tenant's report of the flow whose StableFlowID is flow.
func (a *CachedAnalyzer) InvalidateFlow(flow string) {
	if a == nil || a.reports == nil || flow == "" {
		return
	}
	a.reports.drop(func(e *cachedReport) bool { return e.flow == flow })
}

// Reports lists tenant's cached reports, one per flow, in no particular order: the desktop session analytics.
// Another tenant's reports are never included.
func (a *CachedAnalyzer) Reports(tenant cache.Part) []*models.AnalysisReport {
	if a == nil || a.reports == nil || tenant.IsZero() {
		return nil
	}
	return a.reports.list(tenant)
}

// ResetTenant drops every report of tenant: the desktop's "open a folder" starts a new analytics session.
func (a *CachedAnalyzer) ResetTenant(tenant cache.Part) {
	if a == nil || a.reports == nil || tenant.IsZero() {
		return
	}
	a.reports.drop(func(e *cachedReport) bool { return e.tenant == tenant })
}

// reportKey builds in's report key; false when in is not cacheable here: no report cache, a zero tenant, a
// document without identity, or parts the spec refuses (counted; a panic in a test binary).
func (a *CachedAnalyzer) reportKey(in Input) (reportKey, bool) {
	if a == nil || a.reports == nil || in.Tenant.IsZero() {
		return reportKey{}, false
	}
	flow := StableFlowID(in.Doc)
	if flow == "" {
		return reportKey{}, false
	}
	k, err := reportSpec.Key(in.Tenant, cache.Resource(flow), analyzerVersionPart,
		cache.RuleProfile(ProfileDigest(in.Settings, in.Rules)), contentPart(in.Doc))
	if err != nil {
		return reportKey{}, false
	}
	rk := reportKey{key: k, tenant: in.Tenant, flow: flow}
	if in.Doc.FilePath != "" {
		rk.path = filepath.Clean(in.Doc.FilePath)
	}
	return rk, true
}

// contentPart is a document's content key part: FlowHash, which stays equal across re-parses of the same text, and
// parseIdentity, which does not. A report carries the ids and line numbers of the parse it was computed on, so it is
// served only for that parse (the same document, a copy of it, or a stored flow resolved again with its stored
// ids); a re-parse of unchanged text analyzes again rather than receive another parse's block ids.
func contentPart(doc *models.FlowDocument) cache.Part {
	return cache.Content(FlowHash(doc) + ":" + parseIdentity(doc))
}

// walkReport runs the analysis without per-rule timing (RuleProfiles are a diagnostic, not worth two time.Now()
// calls per block and rule); nil when ctx ended before it completed. The report is stamped with the document's
// FlowHash so history reuses it.
func walkReport(ctx context.Context, in Input) *models.AnalysisReport {
	report, complete := runAnalysisWalk(ctx, in.Doc, in.Rules, in.Settings, in.OnProgress, false)
	if !complete {
		return nil
	}
	if report.ContentHash == "" {
		report.ContentHash = FlowHash(in.Doc)
	}
	return report
}

// errAnalysisIncomplete is the flight error for a walk cut short by its leader's ended context; it never escapes
// Analyze.
var errAnalysisIncomplete = errors.New("analysis cancelled before completion")

// maxAnalysisFlightAttempts bounds re-entries into a flight after joining a cancelled leader.
const maxAnalysisFlightAttempts = 3

// reportKey locates a report: its declared key, and the tenant, flow and path the store compares to keep one
// entry per flow and tenant.
type reportKey struct {
	key    cache.Key
	tenant cache.Part
	flow   string // StableFlowID
	path   string // cleaned FilePath; "" for a path-less document
}

type cachedReport struct {
	report *models.AnalysisReport
	tenant cache.Part
	flow   string
	path   string
	// cost is ReportCost(report) at insertion, charged against the byte budget.
	cost int64
	// seq is the recency stamp, written by get with an atomic under the read lock.
	seq atomic.Int64
}

// reportCache holds the analyzer's reports: one per flow and tenant, bounded by count AND estimated bytes (the
// count cap alone let finding-dense reports pin hundreds of MiB), least recently used out first.
type reportCache struct {
	mu       sync.RWMutex
	entries  map[cache.Key]*cachedReport
	maxSize  int
	maxBytes int64
	bytes    int64
	seq      atomic.Int64
}

func newReportCache(maxSize int, maxBytes int64) *reportCache {
	if maxSize <= 0 {
		maxSize = DefaultReportCacheEntries
	}
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBudget
	}
	c := &reportCache{entries: make(map[cache.Key]*cachedReport), maxSize: maxSize, maxBytes: maxBytes}
	cache.Track(reportSpec, c)
	return c
}

// get returns the report under k and refreshes its recency, so eviction drops the least recently USED entry.
// Callers count the request on the spec.
func (c *reportCache) get(k reportKey) *models.AnalysisReport {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[k.key]
	if !ok {
		return nil
	}
	// Atomic stamp under the read lock: concurrent gets can rarely invert stamps; at worst a marginally less
	// recent entry is evicted.
	e.seq.Store(c.seq.Add(1))
	return e.report
}

// put stores report under k. It first drops the tenant's other entries of the same flow (another content or
// profile) and those whose path overlaps k's (a folder and the files beneath it), so the session analytics count
// each flow once. A report costlier than the whole budget is served but not kept: storing it would evict
// everything else.
func (c *reportCache) put(k reportKey, report *models.AnalysisReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.entries {
		if e.tenant == k.tenant && (e.flow == k.flow || pathsOverlap(k.path, e.path)) {
			c.remove(key, cache.EvictReplaced)
		}
	}
	cost := ReportCost(report)
	if cost > c.maxBytes {
		return
	}
	// Make room by count and by bytes, least recently used first.
	for len(c.entries) > 0 && (len(c.entries) >= c.maxSize || c.bytes+cost > c.maxBytes) {
		var oldest cache.Key
		oldestSeq := int64(1<<63 - 1)
		for key, e := range c.entries {
			if s := e.seq.Load(); s < oldestSeq {
				oldestSeq, oldest = s, key
			}
		}
		if oldest.IsZero() {
			break
		}
		c.remove(oldest, cache.EvictCapacity)
	}
	e := &cachedReport{report: report, tenant: k.tenant, flow: k.flow, path: k.path, cost: cost}
	e.seq.Store(c.seq.Add(1))
	c.entries[k.key] = e
	c.bytes += cost
	reportSpec.RecordStore()
}

// remove deletes key and refunds its cost. Caller holds c.mu.
func (c *reportCache) remove(key cache.Key, why cache.EvictReason) {
	if e, ok := c.entries[key]; ok {
		c.bytes -= e.cost
		delete(c.entries, key)
		reportSpec.RecordEviction(why)
	}
}

// drop deletes every entry match selects.
func (c *reportCache) drop(match func(*cachedReport) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.entries {
		if match(e) {
			c.remove(key, cache.EvictDeleted)
		}
	}
}

// list returns tenant's reports.
func (c *reportCache) list(tenant cache.Part) []*models.AnalysisReport {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []*models.AnalysisReport
	for _, e := range c.entries {
		if e.tenant == tenant && e.report != nil {
			out = append(out, e.report)
		}
	}
	return out
}

// Len is the number of reports held (the spec's entries gauge).
func (c *reportCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Cost is the estimated bytes the reports keep resident (sum of ReportCost).
func (c *reportCache) Cost() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bytes
}

// pathsOverlap reports whether one path contains the other (folder vs file); equal paths share a flow identity.
func pathsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(b, a+sep) || strings.HasPrefix(a, b+sep)
}

// StableFlowID returns an identity for doc that survives re-parsing (the parser
// mints a fresh doc UUID on every load): path-backed docs hash their FilePath,
// path-less inputs carry StableID, cloud flows keep their ID. Must stay in sync
// with the service-layer history key.
func StableFlowID(doc *models.FlowDocument) string {
	if doc.FilePath != "" {
		return StableFlowIDForPath(doc.FilePath)
	}
	if doc.StableID != "" {
		return doc.StableID
	}
	return doc.ID
}

// StableFlowIDForPath is the path-derived form of StableFlowID; the path is cleaned first so "/x/y" and "/x/y/" share one identity.
func StableFlowIDForPath(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return "path-" + hex.EncodeToString(sum[:8])
}

// FlowHash returns doc's content identity (persisted as ContentHash; with parseIdentity, the content part of the
// analyzer's cache keys); memoized.
func FlowHash(doc *models.FlowDocument) string {
	return flowHashes.lookup(doc).hash
}

// parseIdentity digests what a report carries from doc's parse besides its content: the document's id and name,
// and every subflow id, block id and line number. The parser mints ids per parse, and a whitespace-only edit moves
// line numbers without changing FlowHash.
func parseIdentity(doc *models.FlowDocument) string {
	walk := flowHashes.lookup(doc).ids
	h := sha256.New()
	var b []byte
	b = appendLenPrefixed(b, doc.ID)
	b = appendLenPrefixed(b, doc.Name)
	b = appendLenPrefixed(b, walk)
	h.Write(b)
	var sum [sha256.Size]byte
	return hex.EncodeToString(h.Sum(sum[:0])[:16])
}

// appendLenPrefixed appends "<len>:<s>", so a sequence of fields encodes injectively.
func appendLenPrefixed[S ~string | ~[]byte](b []byte, s S) []byte {
	b = strconv.AppendInt(b, int64(len(s)), 10)
	b = append(b, ':')
	return append(b, s...)
}

// Buffer flush thresholds of flowHasher's two streams; SHA-256 is streaming, so chunking never changes a digest.
const (
	flowHashFlushAt = 32 << 10
	flowIDsFlushAt  = 8 << 10
)

// flowHasher feeds the FlowHash byte stream and the parse-identity stream to SHA-256 through one reusable buffer
// each (O(1) per block).
type flowHasher struct {
	h      hash.Hash
	buf    []byte
	ids    hash.Hash
	idsBuf []byte
	keys   []string
}

func (fh *flowHasher) str(s string) {
	fh.buf = append(fh.buf, s...)
}

func (fh *flowHasher) sep(c byte) {
	fh.buf = append(fh.buf, c)
	if len(fh.buf) >= flowHashFlushAt {
		fh.h.Write(fh.buf)
		fh.buf = fh.buf[:0]
	}
}

// id appends one length-prefixed field to the parse-identity stream.
func (fh *flowHasher) id(s string) {
	fh.idsBuf = appendLenPrefixed(fh.idsBuf, s)
	if len(fh.idsBuf) >= flowIDsFlushAt {
		fh.ids.Write(fh.idsBuf)
		fh.idsBuf = fh.idsBuf[:0]
	}
}

// line appends one line number to the parse-identity stream.
func (fh *flowHasher) line(n int) {
	fh.idsBuf = strconv.AppendInt(fh.idsBuf, int64(n), 10)
	fh.idsBuf = append(fh.idsBuf, ';')
}

// block feeds b and its children; sf is the id of the subflow they sit in.
func (fh *flowHasher) block(b *models.Block, sf string) {
	// b.ID is intentionally excluded from the content stream: the parser mints a fresh UUID per block on every
	// parse, so including it would defeat the cache. It goes to the parse-identity stream.
	fh.str(b.RawType)
	fh.sep(0)
	fh.str(b.Name)
	fh.sep(0)
	fh.buf = strconv.AppendInt(fh.buf, int64(b.Indent), 10)
	fh.sep(0)
	if b.Properties != nil {
		keys := fh.keys[:0]
		for k := range b.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fh.str(k)
			fh.sep('=')
			fh.str(b.Properties[k])
			fh.sep(0)
		}
		fh.keys = keys
	}
	for _, v := range b.Variables {
		fh.str(v)
		fh.sep(0)
	}
	fh.line(b.LineNumber)
	fh.line(b.EndLineNumber)
	// The subflow's id is in the stream once, with the subflow; a block repeats it only when its own differs
	// ('^' same, '!' then its own: neither is a digit, so the fields stay apart).
	if b.SubflowID == sf {
		fh.idsBuf = append(fh.idsBuf, '^')
	} else {
		fh.idsBuf = append(fh.idsBuf, '!')
		fh.id(b.SubflowID)
	}
	fh.id(b.ID)
	for i := range b.Children {
		fh.block(&b.Children[i], sf)
	}
}

// computeFlowHashes is the uncached walk behind FlowHash and parseIdentity: the content hash (pinned by
// TestFlowHash_PinnedValues) and the digest of the walk's ids and line numbers, both from one pass.
func computeFlowHashes(doc *models.FlowDocument) (content, ids string) {
	flowHashComputes.Add(1)
	fh := &flowHasher{
		h: sha256.New(), buf: make([]byte, 0, flowHashFlushAt+512),
		ids: sha256.New(), idsBuf: make([]byte, 0, flowIDsFlushAt+512),
	}
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		fh.str(sf.Name)
		fh.sep(0)
		fh.id(sf.ID)
		for j := range sf.Blocks {
			fh.block(&sf.Blocks[j], sf.ID)
		}
	}

	// Parse errors are part of the content identity: without them two
	// block-less docs of different unparseable files shared FlowHash and
	// StableFlowID, so only the rest of the key kept one caller's report
	// (verbatim snippets) from the other.
	for i := range doc.ParseErrors {
		pe := &doc.ParseErrors[i]
		fh.buf = strconv.AppendInt(fh.buf, int64(pe.Line), 10)
		fh.buf = append(fh.buf, ':')
		fh.buf = strconv.AppendInt(fh.buf, int64(pe.Column), 10)
		fh.sep(0)
		fh.str(pe.Severity)
		fh.sep(0)
		fh.str(pe.Message)
		fh.sep(0)
		fh.str(pe.Snippet)
		fh.sep(0)
	}
	var sum [sha256.Size]byte
	fh.h.Write(fh.buf)
	content = hex.EncodeToString(fh.h.Sum(sum[:0])[:8])
	fh.ids.Write(fh.idsBuf)
	ids = hex.EncodeToString(fh.ids.Sum(sum[:0])[:16])
	return content, ids
}

// flowHashComputes counts uncached FlowHash walks (tests assert memo hits).
var flowHashComputes atomic.Int64

// flowHashMemo memoizes FlowHash and the parse-identity walk per *FlowDocument,
// safe only because a handed-out FlowDocument is immutable (edits re-parse into
// a NEW doc); the entry pins the Subflows/ParseErrors slice headers so
// reassigning either recomputes. Weak keys; cleanup removes the entry once
// collected.
type flowHashMemo struct {
	mu sync.Mutex
	m  map[weak.Pointer[models.FlowDocument]]flowHashEntry
}

type flowHashEntry struct {
	hash  string
	ids   string
	shape flowShape
}

// flowShape identifies the Subflows and ParseErrors slices a memo entry was computed from. The slice identities
// are weak too: a strong pointer into Subflows from this global map would keep a dropped document's whole tree
// alive.
type flowShape struct {
	subflows     weak.Pointer[models.Subflow]
	nSubflows    int
	parseErrors  weak.Pointer[models.ParseError]
	nParseErrors int
}

var flowHashes = &flowHashMemo{m: make(map[weak.Pointer[models.FlowDocument]]flowHashEntry)}

func flowHashShape(doc *models.FlowDocument) flowShape {
	s := flowShape{nSubflows: len(doc.Subflows), nParseErrors: len(doc.ParseErrors)}
	if len(doc.Subflows) > 0 {
		s.subflows = weak.Make(&doc.Subflows[0])
	}
	if len(doc.ParseErrors) > 0 {
		s.parseErrors = weak.Make(&doc.ParseErrors[0])
	}
	return s
}

// lookup returns doc's hashes, walking it only when the memo has none for its current shape.
func (fm *flowHashMemo) lookup(doc *models.FlowDocument) flowHashEntry {
	shape := flowHashShape(doc)
	key := weak.Make(doc)
	fm.mu.Lock()
	e, ok := fm.m[key]
	fm.mu.Unlock()
	if ok && e.shape == shape {
		return e
	}
	e = flowHashEntry{shape: shape}
	e.hash, e.ids = computeFlowHashes(doc)
	fm.put(key, doc, e)
	return e
}

func (fm *flowHashMemo) put(key weak.Pointer[models.FlowDocument], doc *models.FlowDocument, e flowHashEntry) {
	fm.mu.Lock()
	_, existed := fm.m[key]
	fm.m[key] = e
	fm.mu.Unlock()
	if !existed {
		runtime.AddCleanup(doc, fm.forget, key)
	}
}

func (fm *flowHashMemo) forget(key weak.Pointer[models.FlowDocument]) {
	fm.mu.Lock()
	delete(fm.m, key)
	fm.mu.Unlock()
}
