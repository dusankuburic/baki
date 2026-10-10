package metrics

import (
	"runtime"
	"slices"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"pad-core/cache"
)

// metricsTestCache is declared in this test binary only, so the collector has a cache to export.
var metricsTestCache = cache.Declare("metricstest.cache", cache.Keys(cache.KindTenant), cache.ReplicaLocal("test"))

// TestCacheCollector_ExportsDeclaredCaches: every declared cache appears under its name with its statistics, the
// eviction reasons and request results are the closed sets, and the values are the spec's own. Each statistic gets
// its own count, so a value exported under another label shows. (The spec is process-wide, so under -count the
// counters carry the earlier runs: the counts per run are distinct, and so are their multiples.)
func TestCacheCollector_ExportsDeclaredCaches(t *testing.T) {
	before := metricsTestCache.Info().Stats
	c := cache.New[string](metricsTestCache, 100)
	key := func(user string) cache.Key {
		k, err := metricsTestCache.Key(cache.User(user))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	k := key("u")
	c.Get(k)            // miss 1
	c.Set(k, "v", 1, 0) // store 1
	for range 4 {
		c.Get(k) // hits 4
	}
	for range 3 {
		c.Set(k, "v", 10, 0) // replaced 3, stores 4
	}
	for _, u := range []string{"e1", "e2"} {
		c.Set(key(u), "v", 1, time.Nanosecond) // stores 6
	}
	time.Sleep(2 * time.Millisecond)
	for _, u := range []string{"e1", "e2"} {
		c.Get(key(u)) // expired 2, misses 3
	}
	for _, u := range []string{"d1", "d2", "d3", "d4"} {
		c.Set(key(u), "v", 1, 0) // stores 10
		c.Delete(key(u))         // deleted 4
	}
	c.Set(key("big"), "v", 95, 0) // store 11, evicts k (capacity 1)
	func() {
		defer cache.SetKeyErrorPanicForTesting(false)()
		if _, err := metricsTestCache.Key(); err == nil { // rejected 1
			t.Fatal("a key without its tenant was accepted")
		}
	}()
	st := metricsTestCache.Info().Stats
	evictions := func(st cache.Stats) [4]uint64 {
		return [4]uint64{st.Evictions[cache.EvictCapacity], st.Evictions[cache.EvictExpired],
			st.Evictions[cache.EvictReplaced], st.Evictions[cache.EvictDeleted]}
	}
	gotRun := [8]uint64{st.Hits - before.Hits, st.Misses - before.Misses, st.Rejected - before.Rejected, st.Stores - before.Stores}
	for i, e := range evictions(st) {
		gotRun[4+i] = e - evictions(before)[i]
	}
	if wantRun := [8]uint64{4, 3, 1, 11, 1, 2, 3, 4}; gotRun != wantRun {
		t.Fatalf("this run's hits, misses, rejected, stores, then capacity, expired, replaced, deleted evictions = %v, want %v",
			gotRun, wantRun)
	}

	families, err := Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]float64{} // family -> "label=value,..." of the test cache -> value
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			labels, ours := "", false
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "cache" {
					ours = lp.GetValue() == metricsTestCache.Name()
					continue
				}
				labels += lp.GetName() + "=" + lp.GetValue()
			}
			if !ours {
				continue
			}
			if got[mf.GetName()] == nil {
				got[mf.GetName()] = map[string]float64{}
			}
			got[mf.GetName()][labels] = valueOf(m)
		}
	}
	runtime.KeepAlive(c)
	want := map[string]map[string]float64{
		"pad_cache_requests_total": {
			"result=hit": float64(st.Hits), "result=miss": float64(st.Misses), "result=rejected": float64(st.Rejected),
		},
		"pad_cache_stores_total": {"": float64(st.Stores)},
		"pad_cache_evictions_total": {
			"reason=capacity": float64(st.Evictions[cache.EvictCapacity]), "reason=expired": float64(st.Evictions[cache.EvictExpired]),
			"reason=replaced": float64(st.Evictions[cache.EvictReplaced]), "reason=deleted": float64(st.Evictions[cache.EvictDeleted]),
		},
		"pad_cache_entries":    {"": -1}, // checked below: earlier runs' caches may be collected meanwhile
		"pad_cache_cost_bytes": {"": -1},
	}
	for family, series := range want {
		for labels, v := range series {
			if g, ok := got[family][labels]; !ok || (v >= 0 && g != v) {
				t.Errorf("%s{cache=%q %s} = %v (present %v), want %v", family, metricsTestCache.Name(), labels, g, ok, v)
			}
		}
		if len(got[family]) != len(series) {
			t.Errorf("%s has series %v, want exactly %v", family, keysOf(got[family]), keysOf(series))
		}
	}
	// This run's cache holds one entry at cost 95, and nothing beyond what the spec counts.
	if e := got["pad_cache_entries"][""]; e < 1 || e > float64(st.Entries) {
		t.Errorf("pad_cache_entries = %v, want between 1 and %d", e, st.Entries)
	}
	if c := got["pad_cache_cost_bytes"][""]; c < 95 || c > float64(st.Cost) {
		t.Errorf("pad_cache_cost_bytes = %v, want between 95 and %d", c, st.Cost)
	}
}

func valueOf(m *dto.Metric) float64 {
	switch {
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	}
	return -1
}

func keysOf(m map[string]float64) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
