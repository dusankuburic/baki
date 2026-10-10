package database

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// The Postgres backend's metrics, registered from this package. The pool gauges stay in internal/metrics: the pool
// loop that records them runs in internal/app.
var (
	blobOps = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_blob_operations_total",
			Help: "Number of Azure Blob Storage operations by type and outcome",
		},
		[]string{"op", "status"},
	)
	blobOpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pad_blob_operation_duration_seconds",
			Help:    "Duration of Azure Blob Storage operations by type",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"op"},
	)
	blobContentMissing = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pad_blob_content_missing_total",
			Help: "Flows whose content blob was absent though metadata recorded content — data loss, surfaced so ops can alert rather than only find a log line.",
		},
	)
	ragFallbackCappedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pad_rag_fallback_capped_total",
			Help: "Knowledge searches served by the Go-side ranking fallback at its 500-chunk sample cap — chunks beyond the deterministic sample are unsearchable; install pgvector or reduce the KB.",
		},
	)
	// Queue depth: surfaces backlog growth (bulk import) so it's visible before
	// it becomes a problem. A gauge (Set on each change).
	blobCleanerPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_blob_cleaner_pending",
		Help: "Deferred blob-cleanup jobs waiting in the cleaner's pending heap. Sustained growth under bulk import/migration signals the worker pool can't keep up.",
	})
)

func init() {
	metrics.MustRegister(blobOps, blobOpDuration, blobContentMissing, ragFallbackCappedTotal, blobCleanerPending)
}

// recordBlobOp records the outcome and duration of an Azure Blob Storage
// operation (op "upload"/"download"/"delete"/"list"; status "ok"/"not_found"/
// "throttled"/"error") — surfaces blob latency, 404s (potential data loss)
// and 429 throttling.
func recordBlobOp(op, status string, dur time.Duration) {
	blobOps.WithLabelValues(op, status).Inc()
	blobOpDuration.WithLabelValues(op).Observe(dur.Seconds())
}

// recordBlobContentMissing bumps the counter for a flow whose content blob was
// absent though its metadata recorded content — a data-loss signal. See
// pad_blob_content_missing_total.
func recordBlobContentMissing() {
	blobContentMissing.Inc()
}

// recordRAGFallbackCapped bumps rag_fallback_capped_total: a knowledge search
// ran on the Go-side fallback at its sample cap, so results are lossy here.
func recordRAGFallbackCapped() {
	ragFallbackCappedTotal.Inc()
}

// setBlobCleanerPending sets the blob-cleaner pending-heap depth gauge.
func setBlobCleanerPending(n int) {
	blobCleanerPending.Set(float64(n))
}
