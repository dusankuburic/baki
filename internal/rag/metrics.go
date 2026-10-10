package rag

import (
	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// ragDimMismatchTotal is the knowledge search's metric, registered from this package.
var ragDimMismatchTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "pad_rag_dim_mismatch_total",
		Help: "Knowledge searches that returned nothing while the org HAS chunks — corpus is stranded at a different embedding dimension than the configured one; re-index.",
	},
)

// ragStrandedSearchTotal counts the searches that found nothing in a corpus the org has because its chunks were
// embedded under another identity than the searcher's (ADR 0027): the same-width model switch the width metric
// above cannot see.
var ragStrandedSearchTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "pad_rag_stranded_search_total",
		Help: "Knowledge searches that returned nothing while the org has chunks embedded under another embedding identity than the searcher's — a model switch at the same width; re-index.",
	},
)

func init() { metrics.MustRegister(ragDimMismatchTotal, ragStrandedSearchTotal) }

// recordRAGStrandedSearch bumps pad_rag_stranded_search_total.
func recordRAGStrandedSearch() {
	ragStrandedSearchTotal.Inc()
}

// recordRAGDimMismatch bumps rag_dim_mismatch_total: a knowledge search found
// nothing while the org has chunks — the corpus is stranded at a different
// embedding dimension than the query's.
func recordRAGDimMismatch() {
	ragDimMismatchTotal.Inc()
}
