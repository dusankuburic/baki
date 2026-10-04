package analyzer

import (
	"golang.org/x/sync/singleflight"

	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	"pad-core/models"
)

type CachedReport struct {
	Hash      string                 `json:"hash"`
	Report    *models.AnalysisReport `json:"report"`
	CreatedAt time.Time              `json:"createdAt"`
	// seq is the recency stamp, written by Get with an atomic (no cache lock).
	seq atomic.Int64
	// Path is the cleaned on-disk path ("" for path-less docs), kept so Put evicts overlapping identities.
	Path string `json:"-"`
	// cost is ReportCost(Report) at insertion, charged against the byte budget.
	cost int64
}

// AnalysisCache memoizes reports by (flow identity, content/config hash),
// bounded by count AND estimated bytes — the count cap alone let finding-dense
// reports pin hundreds of MiB.
type AnalysisCache struct {
	mu       sync.RWMutex
	entries  map[string]*CachedReport
	maxSize  int
	maxBytes int64
	bytes    int64
	seq      atomic.Int64
}

// DefaultCacheBudget is the byte budget of DefaultCache and every
// NewAnalysisCache: ~2x the measured per-finding size, enough for hundreds of
// typical reports or one finding-dense flow in a 1 GiB container.
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

func NewAnalysisCache(maxSize int) *AnalysisCache {
	return NewAnalysisCacheSized(maxSize, DefaultCacheBudget)
}

// NewAnalysisCacheSized is NewAnalysisCache with an explicit byte budget
// (<= 0 selects DefaultCacheBudget); a costlier-than-budget report is served but never stored.
func NewAnalysisCacheSized(maxSize int, maxBytes int64) *AnalysisCache {
	if maxSize <= 0 {
		maxSize = 50
	}
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBudget
	}
	return &AnalysisCache{
		entries:  make(map[string]*CachedReport),
		maxSize:  maxSize,
		maxBytes: maxBytes,
	}
}

// FlowHash returns doc's content identity (the cache key's content component, persisted as ContentHash); memoized.
func FlowHash(doc *models.FlowDocument) string {
	if h, ok := flowHashes.get(doc); ok {
		return h
	}
	h := computeFlowHash(doc)
	flowHashes.put(doc, h)
	return h
}

// flowHashFlushAt bounds flowHasher's buffer; SHA-256 is streaming, so chunking never changes the digest.
const flowHashFlushAt = 32 << 10

// flowHasher feeds FlowHash's byte stream to SHA-256 through one reusable buffer (O(1) per block).
type flowHasher struct {
	h    hash.Hash
	buf  []byte
	keys []string
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

func (fh *flowHasher) block(b *models.Block) {
	// b.ID is intentionally excluded: the parser mints a fresh UUID per block
	// on every parse, so including it would defeat the cache.
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
	for i := range b.Children {
		fh.block(&b.Children[i])
	}
}

// computeFlowHash is the uncached FlowHash walk; pinned by TestFlowHash_PinnedValues.
func computeFlowHash(doc *models.FlowDocument) string {
	flowHashComputes.Add(1)
	fh := &flowHasher{h: sha256.New(), buf: make([]byte, 0, flowHashFlushAt+512)}
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		fh.str(sf.Name)
		fh.sep(0)
		for j := range sf.Blocks {
			fh.block(&sf.Blocks[j])
		}
	}

	// Parse errors are part of the content identity: without them two
	// block-less docs of different unparseable files collided on the
	// tenant-less DefaultCache key, serving one caller's report (verbatim
	// snippets) to the other.
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
	fh.h.Write(fh.buf)
	var sum [sha256.Size]byte
	fh.h.Sum(sum[:0])
	return hex.EncodeToString(sum[:8])
}

// flowHashComputes counts uncached FlowHash walks (tests assert memo hits).
var flowHashComputes atomic.Int64

// flowHashMemo memoizes FlowHash per *FlowDocument, safe only because a
// handed-out FlowDocument is immutable (edits re-parse into a NEW doc); the
// entry pins the Subflows/ParseErrors slice headers so reassigning either
// recomputes. Weak keys; cleanup removes the entry once collected.
type flowHashMemo struct {
	mu sync.Mutex
	m  map[weak.Pointer[models.FlowDocument]]flowHashEntry
}

// The slice identities are weak too: a strong pointer into Subflows from
// this global map would keep a dropped document's whole tree alive.
type flowHashEntry struct {
	hash         string
	subflows     weak.Pointer[models.Subflow]
	nSubflows    int
	parseErrors  weak.Pointer[models.ParseError]
	nParseErrors int
}

var flowHashes = &flowHashMemo{m: make(map[weak.Pointer[models.FlowDocument]]flowHashEntry)}

func flowHashShape(doc *models.FlowDocument) flowHashEntry {
	e := flowHashEntry{nSubflows: len(doc.Subflows), nParseErrors: len(doc.ParseErrors)}
	if len(doc.Subflows) > 0 {
		e.subflows = weak.Make(&doc.Subflows[0])
	}
	if len(doc.ParseErrors) > 0 {
		e.parseErrors = weak.Make(&doc.ParseErrors[0])
	}
	return e
}

func (fm *flowHashMemo) get(doc *models.FlowDocument) (string, bool) {
	shape := flowHashShape(doc)
	key := weak.Make(doc)
	fm.mu.Lock()
	e, ok := fm.m[key]
	fm.mu.Unlock()
	if !ok {
		return "", false
	}
	shape.hash = e.hash
	if shape != e {
		return "", false
	}
	return e.hash, true
}

func (fm *flowHashMemo) put(doc *models.FlowDocument, hash string) {
	e := flowHashShape(doc)
	e.hash = hash
	key := weak.Make(doc)
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

// Get returns the cached report; a hit refreshes recency, so eviction drops the least recently USED entry.
func (c *AnalysisCache) Get(flowID, hash string) *models.AnalysisReport {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[cacheKey(flowID, hash)]
	if !ok {
		return nil
	}
	// Atomic stamp under the read lock: concurrent Gets can rarely invert stamps — at worst a marginally less-recent entry is evicted.
	entry.seq.Store(c.seq.Add(1))
	return entry.Report
}

func (c *AnalysisCache) Put(flowID, hash string, report *models.AnalysisReport) {
	c.PutWithPath(flowID, "", hash, report)
}

// PutWithPath stores a report and evicts overlapping identities: a
// folder-combined doc replaces the per-file entries beneath it and vice
// versa, else the dashboards count the same findings twice.
func (c *AnalysisCache) PutWithPath(flowID, path, hash string, report *models.AnalysisReport) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if path != "" {
		path = filepath.Clean(path)
	}

	// One entry per flow identity: drop prior hashes so the dashboards count each flow exactly once.
	prefix := flowID + ":"
	for k, v := range c.entries {
		if strings.HasPrefix(k, prefix) || pathsOverlap(path, v.Path) {
			c.remove(k)
		}
	}

	// A report that cannot fit the budget on its own is served but not retained: storing it would evict everything else.
	cost := ReportCost(report)
	if cost > c.maxBytes {
		return
	}

	// Make room by count and by bytes, least recently used first.
	for len(c.entries) > 0 && (len(c.entries) >= c.maxSize || c.bytes+cost > c.maxBytes) {
		oldest := ""
		var oldestSeq int64 = 1<<63 - 1
		for k, v := range c.entries {
			if v.seq.Load() < oldestSeq {
				oldestSeq = v.seq.Load()
				oldest = k
			}
		}
		if oldest == "" {
			break
		}
		c.remove(oldest)
	}

	entry := &CachedReport{
		Hash:      hash,
		Report:    report,
		CreatedAt: time.Now(),
		Path:      path,
		cost:      cost,
	}
	entry.seq.Store(c.seq.Add(1))
	c.entries[cacheKey(flowID, hash)] = entry
	c.bytes += cost
}

// remove deletes key and refunds its cost. Caller holds c.mu.
func (c *AnalysisCache) remove(key string) {
	if e, ok := c.entries[key]; ok {
		c.bytes -= e.cost
		delete(c.entries, key)
	}
}

// Bytes reports the estimated retained bytes (sum of ReportCost).
func (c *AnalysisCache) Bytes() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bytes
}

// pathsOverlap reports whether one path contains the other (folder vs file); equal paths are handled by the flowID prefix delete.
func pathsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(b, a+sep) || strings.HasPrefix(a, b+sep)
}

func (c *AnalysisCache) Invalidate(flowID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := flowID + ":"
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) {
			c.remove(k)
		}
	}
}

// Clear removes every cached report (resets desktop session analytics on a new file/folder load).
func (c *AnalysisCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*CachedReport)
	c.bytes = 0
	c.seq.Store(0)
}

func (c *AnalysisCache) AllReports() []*models.AnalysisReport {
	c.mu.RLock()
	defer c.mu.RUnlock()
	reports := make([]*models.AnalysisReport, 0, len(c.entries))
	for _, v := range c.entries {
		if v.Report != nil {
			reports = append(reports, v.Report)
		}
	}
	return reports
}

func cacheKey(flowID, hash string) string {
	return flowID + ":" + hash
}

var DefaultCache = NewAnalysisCache(50)

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

// analyzerVersion participates in the analysis cache key (not FlowHash, the
// pure content identity); bump when rule logic or parser output changes.
const analyzerVersion = "3"

// settingsDigest folds the analysis-relevant settings into the cache key:
// without it, toggling a rule would keep serving the report computed under
// the old config (nothing calls Invalidate on settings changes).
func settingsDigest(settings *models.AppSettings) string {
	if settings == nil || len(settings.Analysis.Rules) == 0 {
		return "default"
	}
	h := sha256.New()
	ids := make([]string, 0, len(settings.Analysis.Rules))
	for id := range settings.Analysis.Rules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rc := settings.Analysis.Rules[id]
		fmt.Fprintf(h, "%s|%t|%s|", id, rc.Enabled, rc.Severity)
		if rc.Options != nil {
			keys := make([]string, 0, len(rc.Options))
			for k := range rc.Options {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(h, "%s=%v;", k, rc.Options[k])
			}
		}
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:8]
}

// RuleDigester is implemented by rules whose behaviour comes from CONFIGURATION
// rather than compiled code (CustomRule): same ID, different configuration.
type RuleDigester interface {
	RuleDigest() string
}

// ruleSetDigest folds the ACTIVE RULE SET into the cache key: StableFlowID + FlowHash can collide across
// tenants and settingsDigest excludes custom rules — deployment-global rules make this dedup, per-org
// rules would make it a cross-tenant leak (TestCachedAnalysis_RuleSetParticipatesInKey). Sorted, so a
// reordered set shares one entry.
func ruleSetDigest(rules []Rule) string {
	if len(rules) == 0 {
		// Not "the default set": runAnalysisCore iterates exactly what it is given.
		return "none"
	}
	ids := make([]string, 0, len(rules))
	for _, r := range rules {
		if d, ok := r.(RuleDigester); ok {
			ids = append(ids, r.ID()+"@"+d.RuleDigest())
			continue
		}
		ids = append(ids, r.ID())
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		io.WriteString(h, id)
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:8]
}

func CachedAnalysis(doc *models.FlowDocument, rules []Rule, settings *models.AppSettings, onProgress func(int, int, string)) *models.AnalysisReport {
	return CachedAnalysisCtx(context.Background(), doc, rules, settings, onProgress)
}

// CachedAnalysisCtx is CachedAnalysis with cancellation. Returns nil only when
// gctx was cancelled before the walk finished — a truncated report is never
// cached or handed out; callers check gctx.Err() on nil.
func CachedAnalysisCtx(gctx context.Context, doc *models.FlowDocument, rules []Rule, settings *models.AppSettings, onProgress func(int, int, string)) *models.AnalysisReport {
	if gctx == nil {
		gctx = context.Background()
	}
	id, hash, flowHash := cacheIdentity(doc, rules, settings)
	if cached := DefaultCache.Get(id, hash); cached != nil {
		return cached
	}

	// analyzeAndStore runs the walk under gctx and caches the report only if it completed.
	analyzeAndStore := func() (*models.AnalysisReport, error) {
		// Skip per-rule timing on the cached hot path: RuleProfiles are a dev
		// diagnostic, not worth two time.Now() calls per (block, rule).
		report, complete := runAnalysisWalk(gctx, doc, rules, settings, onProgress, false)
		if !complete {
			return nil, errAnalysisIncomplete
		}
		// Pin the content hash so downstream consumers reuse it instead of re-walking for FlowHash.
		if report.ContentHash == "" {
			report.ContentHash = flowHash
		}
		DefaultCache.PutWithPath(id, doc.FilePath, hash, report)
		return report, nil
	}

	// In-flight dedup for racing callers. Joiners run under the LEADER's
	// context: a cancelled leader fails the flight with errAnalysisIncomplete,
	// and a live joiner starts a fresh flight rather than receiving a truncated report.
	sfKey := id + "\x00" + hash
	for attempt := 0; attempt < maxAnalysisFlightAttempts; attempt++ {
		v, err, _ := analysisSF.Do(sfKey, func() (any, error) {
			// Re-check inside the flight: a previous leader may have filled the cache between our miss and the slot.
			if cached := DefaultCache.Get(id, hash); cached != nil {
				return cached, nil
			}
			return analyzeAndStore()
		})
		if err == nil && v != nil {
			return v.(*models.AnalysisReport)
		}
		if gctx.Err() != nil {
			return nil
		}
		if !errors.Is(err, errAnalysisIncomplete) {
			break
		}
	}
	// Repeatedly joined cancelled leaders: walk directly under our own context.
	report, err := analyzeAndStore()
	if err != nil {
		return nil
	}
	return report
}

// cacheIdentity computes DefaultCache's key: the flow identity, the full hash
// (analyzer version, content, settings digest, rule-set digest) and the bare
// content hash the report is stamped with.
func cacheIdentity(doc *models.FlowDocument, rules []Rule, settings *models.AppSettings) (id, hash, flowHash string) {
	flowHash = FlowHash(doc)
	hash = analyzerVersion + ":" + flowHash + ":" + settingsDigest(settings) + ":" + ruleSetDigest(rules)
	return StableFlowID(doc), hash, flowHash
}

// LookupCachedReport returns the report DefaultCache already holds for doc
// under rules and settings: a hit must never queue behind running walks (the
// large-analysis semaphore) and refreshes recency like any Get.
func LookupCachedReport(doc *models.FlowDocument, rules []Rule, settings *models.AppSettings) *models.AnalysisReport {
	id, hash, _ := cacheIdentity(doc, rules, settings)
	return DefaultCache.Get(id, hash)
}

// errAnalysisIncomplete is the singleflight error for a walk cut short by its leader's cancelled context; it never escapes CachedAnalysisCtx.
var errAnalysisIncomplete = errors.New("analysis cancelled before completion")

// maxAnalysisFlightAttempts bounds re-entries into the singleflight after joining a cancelled leader.
const maxAnalysisFlightAttempts = 3

// analysisSF dedups concurrent identical analysis walks.
var analysisSF singleflight.Group
