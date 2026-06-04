package analyzer

import (
	"crypto/sha256"
	"fmt"
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
		h.Write([]byte(sf.Name))
		h.Write([]byte{0})
		walkSubflowBlocks(&sf, func(b *models.Block) {
			h.Write([]byte(b.ID))
			h.Write([]byte{0})
			h.Write([]byte(b.RawType))
			h.Write([]byte{0})
			h.Write([]byte(b.Name))
			h.Write([]byte{0})
			if b.Properties != nil {
				keys := make([]string, 0, len(b.Properties))
				for k := range b.Properties {
					keys = append(keys, k)
				}
				// Sort for a deterministic hash — Go randomizes map iteration
				// order, so without this the same flow hashes differently on
				// each call and the analysis cache never hits.
				sort.Strings(keys)
				for _, k := range keys {
					h.Write([]byte(k))
					h.Write([]byte{'='})
					h.Write([]byte(b.Properties[k]))
					h.Write([]byte{0})
				}
			}
			for _, v := range b.Variables {
				h.Write([]byte(v))
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

func CachedAnalysis(doc *models.FlowDocument, rules []Rule, settings *models.AppSettings, onProgress func(int, int, string)) *models.AnalysisReport {
	hash := FlowHash(doc)
	if cached := DefaultCache.Get(doc.ID, hash); cached != nil {
		return cached
	}

	// Skip per-rule timing on the cached hot path; RuleProfiles durations are a
	// dev diagnostic and not worth two time.Now() calls per (block, rule) here.
	report := runAnalysisCore(doc, rules, settings, onProgress, false)
	DefaultCache.Put(doc.ID, hash, report)
	return report
}
