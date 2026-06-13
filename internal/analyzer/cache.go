package analyzer

import (
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"pad-analyzer/internal/models"
)

type CachedReport struct {
	Hash      string                `json:"hash"`
	Report    *models.AnalysisReport `json:"report"`
	CreatedAt time.Time             `json:"createdAt"`
	Seq       int64                 `json:"-"`
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
			io.WriteString(h, b.ID)
			h.Write([]byte{0})
			io.WriteString(h, b.RawType)
			h.Write([]byte{0})
			io.WriteString(h, b.Name)
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
	c.mu.Lock()
	defer c.mu.Unlock()

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
	}
	c.seq++
}

func (c *AnalysisCache) Invalidate(flowID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if len(k) > len(flowID) && k[:len(flowID)] == flowID {
			delete(c.entries, k)
		}
	}
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
	if cached := DefaultCache.Get(doc.ID, hash); cached != nil {
		return cached
	}

	// Skip per-rule timing on the cached hot path; RuleProfiles durations are a
	// dev diagnostic and not worth two time.Now() calls per (block, rule) here.
	report := runAnalysisCore(doc, rules, settings, onProgress, false)
	DefaultCache.Put(doc.ID, hash, report)
	return report
}
