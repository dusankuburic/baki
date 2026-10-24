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

// ragScrubbedChunksTotal counts the knowledge chunks in which the scrubber masked a secret pattern before the text
// went to the embedding provider, by source: upload (the document is scrubbed before chunking, ADR 0027 §10) and
// reindex (a stored chunk whose text changed under the scrub — raw text from before uploads were masked, which the
// re-index leaves in the store; a count here says the org has documents to re-upload).
var ragScrubbedChunksTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "pad_rag_scrubbed_chunks_total",
		Help: "Knowledge chunks in which a secret pattern was masked before embedding, by source (upload: the document scrubbed before chunking; reindex: a stored chunk whose text changed under the scrub — raw text from before uploads were masked, still raw in the store until the document is re-uploaded).",
	},
	[]string{"source"},
)

func init() {
	metrics.MustRegister(ragDimMismatchTotal, ragStrandedSearchTotal, ragScrubbedChunksTotal)
}

// recordRAGScrubbedChunks adds n masked chunks to pad_rag_scrubbed_chunks_total for source (upload, reindex).
func recordRAGScrubbedChunks(source string, n int) {
	if n > 0 {
		ragScrubbedChunksTotal.WithLabelValues(source).Add(float64(n))
	}
}

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
