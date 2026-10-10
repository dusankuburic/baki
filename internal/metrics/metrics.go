// Package metrics owns the Prometheus registry /metrics serves. Leaf package: every package that records a metric
// imports it, and it imports none of them. A package defines its metrics itself, beside the code that records them,
// and registers them with MustRegister at package initialisation; testdata/metrics.golden lists every registered
// family and the package that registered it. Left here: the registry, the cache and runtime collectors, the
// background job metrics (jobs.go) and the few metrics onBehalf lists.
package metrics

import (
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

var (
	postgresPoolInUse = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "postgres_pool_in_use",
		Help: "Current Postgres connections checked out of the pool.",
	})
	postgresPoolIdle = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "postgres_pool_idle",
		Help: "Current Postgres connections idle in the pool.",
	})
	postgresPoolOpen = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "postgres_pool_open",
		Help: "Current total Postgres connections (in use + idle).",
	})
	// The three below mirror database/sql's cumulative DBStats counters as
	// gauges set on every snapshot (the pool owns the counters; a Prometheus
	// Counter could only be fed deltas). Monotonic for the life of the
	// process, so rate()/increase() apply. A rising wait count is pool
	// exhaustion; a rising closed{reason="max_idle"} is idle-cap churn.
	postgresPoolWaitCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "postgres_pool_wait_count",
		Help: "Cumulative number of Postgres connection requests that had to wait for a free pooled connection (database/sql DBStats.WaitCount).",
	})
	postgresPoolWaitSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "postgres_pool_wait_duration_seconds",
		Help: "Cumulative time spent waiting for a free pooled Postgres connection (database/sql DBStats.WaitDuration).",
	})
	postgresPoolClosed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgres_pool_closed_connections",
		Help: "Cumulative Postgres connections closed by the pool, by reason: max_idle (idle cap exceeded on return), max_idle_time (ConnMaxIdleTime), max_lifetime (ConnMaxLifetime).",
	}, []string{"reason"})
	backgroundLoopTick = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_background_loop_tick_total",
			Help: "Iterations of each long-running periodic background loop (scanner, retention_purge). Stops increasing ⇒ the loop has hung. Recommended alert: time() - pad_background_loop_last_tick_timestamp_seconds > 2 * expected_interval. Worker-pool loops (blob_cleaner, audit_pool) are not periodic — they're driven by enqueue — so are not labelled here.",
		},
		[]string{"loop"},
	)
	backgroundLoopLastTick = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pad_background_loop_last_tick_timestamp_seconds",
			Help: "Unix timestamp of the last completed iteration of each background loop. Stops updating ⇒ the loop has hung. Pair with pad_background_loop_tick_total to detect deadlocks /healthz cannot see.",
		},
		[]string{"loop"},
	)
	backgroundDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_background_dropped_total",
			Help: "Best-effort background tasks (analysis webhooks, finding emails) not started because their concurrency cap was reached or shutdown had begun, by task and reason.",
		},
		[]string{"task", "reason"},
	)
)

// registry is the one registry /metrics serves. It is process-local, not the client library's global one, so tests
// are hermetic and /metrics shows exactly what is registered here. Every collector reaches it through Register or
// MustRegister from a package-level var or an init function, and it records the package that registered each, for
// the metrics golden (testdata/metrics.golden).
var registry = newModuleRegistry()

// onBehalf are the collectors this package registers for the packages that record them: what is left of the central
// list that predates Register. The pool gauges are recorded from internal/app, the loop ticks from internal/app and
// internal/scanner, and pad_background_dropped_total from internal/lifecycle, internal/mail and internal/websocket
// (which does not import internal/lifecycle). The list only shrinks (centralListPin), and a new metric registers from
// its own package.
var onBehalf = []prometheus.Collector{
	postgresPoolInUse,
	postgresPoolIdle,
	postgresPoolOpen,
	postgresPoolWaitCount,
	postgresPoolWaitSeconds,
	postgresPoolClosed,
	backgroundLoopTick,
	backgroundLoopLastTick,
	backgroundDroppedTotal,
}

func init() {
	// This package's own: the declared caches' statistics and the client library's Go runtime and process collectors.
	MustRegister(cacheStats, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	MustRegister(onBehalf...)
}

// Register adds collectors to the registry /metrics serves and records the package whose code called it. Call it
// from a package-level var or an init function of the package that records the metric, so the metric is served, and
// listed in the golden, before anything records it (TestRegistration_OnlyThroughThePackageRegistry pins this):
//
//	var flowsScanned = prometheus.NewCounter(prometheus.CounterOpts{Name: "pad_scanner_flows_total", Help: "…"})
//
//	func init() { metrics.MustRegister(flowsScanned) }
//
// Registering a collector twice, or another under a registered name, is an error; the others are registered.
func Register(cs ...prometheus.Collector) error { return registry.register(callerPackage(), cs) }

// MustRegister is Register panicking on an error, for package initialisation.
func MustRegister(cs ...prometheus.Collector) { registry.mustRegister(callerPackage(), cs) }

// moduleRegistry is a prometheus.Registry that remembers which package registered each collector.
type moduleRegistry struct {
	reg *prometheus.Registry
	mu  sync.Mutex
	all []registration
}

// registration is one registered collector and the import path of the package that registered it.
type registration struct {
	collector prometheus.Collector
	pkg       string
}

func newModuleRegistry() *moduleRegistry { return &moduleRegistry{reg: prometheus.NewRegistry()} }

// MustRegister serves this package's own files, which register like any other package (registry.MustRegister in an
// init function).
func (r *moduleRegistry) MustRegister(cs ...prometheus.Collector) {
	r.mustRegister(callerPackage(), cs)
}

// Gather implements prometheus.Gatherer.
func (r *moduleRegistry) Gather() ([]*dto.MetricFamily, error) { return r.reg.Gather() }

func (r *moduleRegistry) mustRegister(pkg string, cs []prometheus.Collector) {
	if err := r.register(pkg, cs); err != nil {
		panic(err)
	}
}

func (r *moduleRegistry) register(pkg string, cs []prometheus.Collector) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for _, c := range cs {
		if err := r.reg.Register(c); err != nil {
			errs = append(errs, err)
			continue
		}
		r.all = append(r.all, registration{c, pkg})
	}
	if len(errs) == 1 {
		return errs[0] // unjoined, so a type assertion on prometheus.AlreadyRegisteredError still works
	}
	return errors.Join(errs...)
}

func (r *moduleRegistry) registrations() []registration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.all)
}

// thisPackage is this package's import path.
var thisPackage = reflect.TypeFor[moduleRegistry]().PkgPath()

// callerPackage returns the import path of the package whose code called the registration functions above. It
// walks past them by name rather than counting frames, which inlining would shift.
func callerPackage() string {
	pcs := make([]uintptr, 16)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	for {
		f, more := frames.Next()
		rest, ours := strings.CutPrefix(f.Function, thisPackage+".")
		funnel := ours && (rest == "Register" || rest == "MustRegister" || rest == "callerPackage" ||
			strings.HasPrefix(rest, "(*moduleRegistry)."))
		if !funnel {
			return packageOf(f.Function)
		}
		if !more {
			return ""
		}
	}
}

// packageOf returns the import path in a function name as the runtime spells it ("path/to/pkg.Func",
// "path/to/pkg.(*T).Method.func1").
func packageOf(fn string) string {
	slash := strings.LastIndexByte(fn, '/')
	if dot := strings.IndexByte(fn[slash+1:], '.'); dot >= 0 {
		return fn[:slash+1+dot]
	}
	return fn
}

// Handler returns an http.Handler that serves /metrics in the Prometheus
// exposition format — reachable in production only from the metrics scraper
// (gate at the load balancer or use a private listener if needed).
func Handler() http.Handler {
	return promhttp.HandlerFor(registry.reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		Registry:          registry.reg,
	})
}

// ObservePostgresPool snapshots db.Stats() into the gauge metrics; call
// periodically from main's background goroutine (cheap: atomic reads).
func ObservePostgresPool(db *sql.DB) {
	if db == nil {
		return
	}
	observePoolStats(db.Stats())
}

// Pool-closure reasons: the complete, fixed label set of
// postgres_pool_closed_connections. Every snapshot sets all three.
const (
	poolClosedMaxIdle     = "max_idle"
	poolClosedMaxIdleTime = "max_idle_time"
	poolClosedMaxLifetime = "max_lifetime"
)

// observePoolStats records one DBStats snapshot. Split from
// ObservePostgresPool so it is testable without a live pool.
func observePoolStats(s sql.DBStats) {
	postgresPoolInUse.Set(float64(s.InUse))
	postgresPoolIdle.Set(float64(s.Idle))
	postgresPoolOpen.Set(float64(s.OpenConnections))
	postgresPoolWaitCount.Set(float64(s.WaitCount))
	postgresPoolWaitSeconds.Set(s.WaitDuration.Seconds())
	postgresPoolClosed.WithLabelValues(poolClosedMaxIdle).Set(float64(s.MaxIdleClosed))
	postgresPoolClosed.WithLabelValues(poolClosedMaxIdleTime).Set(float64(s.MaxIdleTimeClosed))
	postgresPoolClosed.WithLabelValues(poolClosedMaxLifetime).Set(float64(s.MaxLifetimeClosed))
}

func counterValue(c prometheus.Counter) float64 {
	m := &dto.Metric{}
	if err := c.Write(m); err != nil || m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// RecordBackgroundLoopTick records a completed iteration of a long-running
// background loop (scanner, retention_purge, …). Recommended alert:
// time() - pad_background_loop_last_tick_timestamp_seconds{loop="X"} >
// 2 * expected_interval — catches hangs that /healthz (always 200) cannot see.
func RecordBackgroundLoopTick(name string) {
	backgroundLoopTick.WithLabelValues(name).Inc()
	backgroundLoopLastTick.WithLabelValues(name).SetToCurrentTime()
}

// RecordBackgroundDropped counts a best-effort background task that was not
// started; task is the runner's code-literal name, reason "saturated" or
// "closed" — both closed sets, never request data.
func RecordBackgroundDropped(task, reason string) {
	backgroundDroppedTotal.WithLabelValues(task, reason).Inc()
}

// BackgroundDroppedCount returns the current pad_background_dropped_total
// value for a task and reason. Exposed for tests; safe for production use.
func BackgroundDroppedCount(task, reason string) float64 {
	m := &dto.Metric{}
	if err := backgroundDroppedTotal.WithLabelValues(task, reason).Write(m); err != nil {
		return 0
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// Gather returns the current metric families from the package registry —
// exported so packages can assert their own instrumentation end to end
// through the real registry (catches a metric defined but never registered,
// or registered but never incremented).
func Gather() ([]*dto.MetricFamily, error) {
	return registry.Gather()
}
