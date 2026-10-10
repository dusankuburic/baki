package errreport

import (
	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// The error funnel's metrics, registered from this package.
var (
	panicRecoveredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_panics_total",
			Help: "Panics recovered (HTTP handlers, background goroutines) forwarded to the error-reporting sink, by location.",
		},
		[]string{"location"},
	)
	errorsReportedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_errors_reported_total",
			Help: "Notable errors forwarded to the error-reporting sink for aggregation/triage, by location.",
		},
		[]string{"location"},
	)
)

func init() { metrics.MustRegister(panicRecoveredTotal, errorsReportedTotal) }

// recordPanic bumps the panics_total counter for a recovered panic at the given
// location (e.g. "http", "scanner", "audit"). Always recorded, even without an
// external error-aggregation sink, so recovered panics surface in the
// Prometheus/alert pipeline regardless.
func recordPanic(location string) {
	panicRecoveredTotal.WithLabelValues(location).Inc()
}

// recordError bumps the errors_reported_total counter for a notable error
// forwarded to the error-reporting sink from the given location.
func recordError(location string) {
	errorsReportedTotal.WithLabelValues(location).Inc()
}
