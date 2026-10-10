package lifecycle

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// The Bounded runners' metrics, registered from this package. pad_background_dropped_total stays in
// internal/metrics: internal/websocket records it too, and does not import this package.
var (
	backgroundInflight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pad_background_inflight",
			Help: "Best-effort background tasks running now, by task. A task pinned at its cap means its target (SMTP, webhook, database) is slow.",
		},
		[]string{"task"},
	)
	backgroundDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pad_background_task_duration_seconds",
			Help:    "Run time of best-effort background tasks, by task.",
			Buckets: []float64{0.01, 0.05, 0.25, 1, 5, 15, 60, 300, 1800},
		},
		[]string{"task"},
	)
)

func init() { metrics.MustRegister(backgroundInflight, backgroundDuration) }

// recordBackgroundStarted counts a background task as running; pair with recordBackgroundFinished.
func recordBackgroundStarted(task string) {
	backgroundInflight.WithLabelValues(task).Inc()
}

// recordBackgroundFinished records a background task's end and run time.
func recordBackgroundFinished(task string, d time.Duration) {
	backgroundInflight.WithLabelValues(task).Dec()
	backgroundDuration.WithLabelValues(task).Observe(d.Seconds())
}
