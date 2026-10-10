package analyzer

import (
	"path/filepath"

	"pad-core/cache"
	"pad-core/models"
)

// newTestAnalyzer is an analyzer with the process's default caches.
func newTestAnalyzer() *CachedAnalyzer {
	return NewCachedAnalyzer(
		WithReportCache(DefaultReportCacheEntries, DefaultCacheBudget),
		WithInventoryCache(DefaultInventoryCacheEntries))
}

// localIn is the desktop's request for doc: the local tenant.
func localIn(doc *models.FlowDocument, rules []Rule, settings *models.AppSettings, onProgress func(int, int, string)) Input {
	return Input{Doc: doc, Rules: rules, Settings: settings, Tenant: cache.Local(), OnProgress: onProgress}
}

// testPathKey is the local tenant's report key of flow at path ("" for none) under content h, with a fixed profile:
// the report store's tests address entries by flow and content alone.
func testPathKey(flow, path, h string) reportKey {
	k, err := reportSpec.Key(cache.Local(), cache.Resource(flow), analyzerVersionPart, cache.RuleProfile("test-profile"), cache.Content(h))
	if err != nil {
		panic(err)
	}
	if path != "" {
		path = filepath.Clean(path)
	}
	return reportKey{key: k, tenant: cache.Local(), flow: flow, path: path}
}

func (c *reportCache) testPut(flow, h string, r *models.AnalysisReport) {
	c.put(testPathKey(flow, "", h), r)
}

func (c *reportCache) testPutPath(flow, path, h string, r *models.AnalysisReport) {
	c.put(testPathKey(flow, path, h), r)
}

func (c *reportCache) testGet(flow, h string) *models.AnalysisReport {
	return c.get(testPathKey(flow, "", h))
}

func (c *reportCache) testAll() []*models.AnalysisReport { return c.list(cache.Local()) }

func (c *reportCache) testInvalidate(flow string) {
	c.drop(func(e *cachedReport) bool { return e.flow == flow })
}
