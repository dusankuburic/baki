package analyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"pad-core/models"
)

type CachedReport struct {
	Hash      string                 `json:"hash"`
	Report    *models.AnalysisReport `json:"report"`
	CreatedAt time.Time              `json:"createdAt"`
	Seq       int64                  `json:"-"`
	// Path is the cleaned on-disk path of the analyzed doc ("" for path-less
	// docs). Kept so Put can evict overlapping identities: a folder-combined
	// doc and its constituent files must never both count in the dashboards.
	Path string `json:"-"`
}

type AnalysisCache struct {
	mu      sync.RWMutex
	entries map[string]*CachedReport
	maxSize int
	seq     int64
}

func NewAnalysisCache(maxSize int) *AnalysisCache {
	if maxSize <= 0 {
		maxSize = 50
	}
	return &AnalysisCache{
		entries: make(map[string]*CachedReport),
		maxSize: maxSize,
	}
}

func FlowHash(doc *models.FlowDocument) string {
	h := sha256.New()
	for _, sf := range doc.Subflows {
		io.WriteString(h, sf.Name)
		h.Write([]byte{0})
		walkSubflowBlocks(&sf, func(b *models.Block) {
			// NOTE: b.ID is intentionally excluded — the parser mints a fresh UUID
			// per block on every parse, so including it would make FlowHash
			// differ across re-parses of byte-identical source and defeat the
			// analysis cache. Structural identity comes from RawType/Name/Indent
			// plus properties/variables and the tree walk order.
			io.WriteString(h, b.RawType)
			h.Write([]byte{0})
			io.WriteString(h, b.Name)
			h.Write([]byte{0})
			fmt.Fprintf(h, "%d", b.Indent)
			h.Write([]byte{0})
			if b.Properties != nil {
				keys := make([]string, 0, len(b.Properties))
				for k := range b.Properties {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					io.WriteString(h, k)
					h.Write([]byte{'='})
					io.WriteString(h, b.Properties[k])
					h.Write([]byte{0})
				}
			}
			for _, v := range b.Variables {
				io.WriteString(h, v)
				h.Write([]byte{0})
			}
		})
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func (c *AnalysisCache) Get(flowID, hash string) *models.AnalysisReport {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[cacheKey(flowID, hash)]
	if !ok {
		return nil
	}
	return entry.Report
}

func (c *AnalysisCache) Put(flowID, hash string, report *models.AnalysisReport) {
	c.PutWithPath(flowID, "", hash, report)
}

// PutWithPath stores a report under flowID and remembers the doc's on-disk
// path so overlapping identities are evicted: a folder-combined doc replaces
// the per-file entries beneath it, and a per-file entry replaces any folder
// aggregate covering it — otherwise the dashboards would count the same
// findings twice (once in the aggregate, once per file).
func (c *AnalysisCache) PutWithPath(flowID, path, hash string, report *models.AnalysisReport) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if path != "" {
		path = filepath.Clean(path)
	}

	// One entry per flow identity: drop prior hashes (older content or settings)
	// so the dashboards, which aggregate AllReports, count each flow exactly once.
	prefix := flowID + ":"
	for k, v := range c.entries {
		if strings.HasPrefix(k, prefix) || pathsOverlap(path, v.Path) {
			delete(c.entries, k)
		}
	}

	if len(c.entries) >= c.maxSize {
		oldest := ""
		var oldestSeq int64 = 1<<63 - 1
		for k, v := range c.entries {
			if v.Seq < oldestSeq {
				oldestSeq = v.Seq
				oldest = k
			}
		}
		if oldest != "" {
			delete(c.entries, oldest)
		}
	}

	c.entries[cacheKey(flowID, hash)] = &CachedReport{
		Hash:      hash,
		Report:    report,
		CreatedAt: time.Now(),
		Seq:       c.seq,
		Path:      path,
	}
	c.seq++
}

// pathsOverlap reports whether one path contains the other (folder vs file
// within it). Equal paths are handled by the flowID prefix delete, not here.
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
			delete(c.entries, k)
		}
	}
}

// Clear removes every cached report. Used to reset the desktop session analytics
// when the user loads a new file or folder, so the dashboards (which aggregate
// AllReports) reflect only the flows analyzed in the current working context.
func (c *AnalysisCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*CachedReport)
	c.seq = 0
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

// StableFlowID returns an identity for doc that survives re-parsing. The parser
// mints a fresh doc UUID on every load, so keying analytics by doc.ID would
// count the same file once per open. Path-backed docs (desktop file/folder
// loads, batch analysis) hash their FilePath; path-less parsed inputs carry a
// parser-assigned StableID (uploads key on their file-name set); everything
// else (cloud library flows) keeps its persistent ID. Must stay in sync with
// the service-layer history key (AnalysisService history/diff), which
// delegates here.
func StableFlowID(doc *models.FlowDocument) string {
	if doc.FilePath != "" {
		return StableFlowIDForPath(doc.FilePath)
	}
	if doc.StableID != "" {
		return doc.StableID
	}
	return doc.ID
}

// StableFlowIDForPath is the path-derived form of StableFlowID, for callers
// that only have a path. The path is cleaned first so "/x/y" and "/x/y/"
// (e.g. a folder picked twice through different adapters) share one identity.
func StableFlowIDForPath(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return "path-" + hex.EncodeToString(sum[:8])
}

// analyzerVersion participates in the analysis cache key (not FlowHash, which
// history.go uses for pure content identity). Bump when rule logic or parser
// output changes so stale cached reports are not served after an upgrade.
const analyzerVersion = "2"

// settingsDigest folds the analysis-relevant settings (rule enabled/severity/
// options) into the cache key. Without it the key is content-only, so toggling
// a rule or changing a threshold would keep serving the report computed under
// the old configuration (nothing calls Invalidate on settings changes).
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

func CachedAnalysis(doc *models.FlowDocument, rules []Rule, settings *models.AppSettings, onProgress func(int, int, string)) *models.AnalysisReport {
	hash := analyzerVersion + ":" + FlowHash(doc) + ":" + settingsDigest(settings)
	id := StableFlowID(doc)
	if cached := DefaultCache.Get(id, hash); cached != nil {
		return cached
	}

	// Skip per-rule timing on the cached hot path; RuleProfiles durations are a
	// dev diagnostic and not worth two time.Now() calls per (block, rule) here.
	report := runAnalysisCore(doc, rules, settings, onProgress, false)
	DefaultCache.PutWithPath(id, doc.FilePath, hash, report)
	return report
}
