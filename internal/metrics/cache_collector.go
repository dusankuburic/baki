package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"pad-core/cache"
)

// Cache statistics (ADR 0017). pad-core has no Prometheus dependency: every declared cache keeps its counts as
// atomics on its spec, and this collector reads them at scrape time. The cache label is a declared spec name, a
// closed set fixed in code (cache.Declare takes a literal at package level, pinned in archtest).
var (
	cacheRequestsDesc = prometheus.NewDesc("pad_cache_requests_total",
		"Lookups of the declared caches, by cache and result: hit, miss, or rejected (the key could not be built, e.g. a missing tenant, so the lookup skipped the cache).",
		[]string{"cache", "result"}, nil)
	cacheStoresDesc = prometheus.NewDesc("pad_cache_stores_total",
		"Values stored in the declared caches, by cache. Misses that store nothing were fenced by an invalidation, too large for the cache, or failed.",
		[]string{"cache"}, nil)
	cacheEvictionsDesc = prometheus.NewDesc("pad_cache_evictions_total",
		"Entries that left the declared caches, by cache and reason: capacity (budget or entry cap), expired (TTL), replaced (stored again under the same key), deleted (invalidated).",
		[]string{"cache", "reason"}, nil)
	cacheEntriesDesc = prometheus.NewDesc("pad_cache_entries",
		"Entries held by the declared caches (every instance of the cache summed; expired entries count until touched).",
		[]string{"cache"}, nil)
	cacheCostDesc = prometheus.NewDesc("pad_cache_cost_bytes",
		"Summed cost of the entries held by the declared caches: bytes for byte-budgeted caches; count-bounded caches charge 1 per entry.",
		[]string{"cache"}, nil)
)

// Results of pad_cache_requests_total: its closed label set.
const (
	cacheResultHit      = "hit"
	cacheResultMiss     = "miss"
	cacheResultRejected = "rejected"
)

// cacheCollector exports the statistics of every declared cache.
type cacheCollector struct {
	specs func() []cache.SpecInfo
}

// cacheStats is the registered collector.
var cacheStats = cacheCollector{specs: cache.Specs}

func (c cacheCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- cacheRequestsDesc
	ch <- cacheStoresDesc
	ch <- cacheEvictionsDesc
	ch <- cacheEntriesDesc
	ch <- cacheCostDesc
}

func (c cacheCollector) Collect(ch chan<- prometheus.Metric) {
	for _, s := range c.specs() {
		st := s.Stats
		ch <- prometheus.MustNewConstMetric(cacheRequestsDesc, prometheus.CounterValue, float64(st.Hits), s.Name, cacheResultHit)
		ch <- prometheus.MustNewConstMetric(cacheRequestsDesc, prometheus.CounterValue, float64(st.Misses), s.Name, cacheResultMiss)
		ch <- prometheus.MustNewConstMetric(cacheRequestsDesc, prometheus.CounterValue, float64(st.Rejected), s.Name, cacheResultRejected)
		ch <- prometheus.MustNewConstMetric(cacheStoresDesc, prometheus.CounterValue, float64(st.Stores), s.Name)
		for _, r := range cache.EvictReasons() {
			ch <- prometheus.MustNewConstMetric(cacheEvictionsDesc, prometheus.CounterValue, float64(st.Evictions[r]), s.Name, r.String())
		}
		ch <- prometheus.MustNewConstMetric(cacheEntriesDesc, prometheus.GaugeValue, float64(st.Entries), s.Name)
		ch <- prometheus.MustNewConstMetric(cacheCostDesc, prometheus.GaugeValue, float64(st.Cost), s.Name)
	}
}
