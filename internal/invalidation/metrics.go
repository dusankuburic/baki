package invalidation

import (
	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// The fan-out's metrics (docs/adr/0032-cache-invalidation-fanout-and-state-registry.md). The label values are
// closed sets written in this package: the message kinds (inval, recheck, hb, resync), the reasons a message was
// dropped or ignored, and the reasons a resync ran. Nothing a peer sends becomes a label. What the decoder made of
// each payload (ok, unknown, bad) is pad_redis_msgs_total{kind}, which internal/redisx owns.
var (
	published = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_inval_published_total",
		Help: "Messages this replica put on the invalidation channel, by kind: inval (a cache to drop), recheck (sockets and streams to re-check), hb (heartbeat naming the last epoch sent) and resync (drop everything: the queue overflowed).",
	}, []string{"kind"})
	received = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_inval_received_total",
		Help: "Messages from other replicas this replica understood and acted on, by kind (as pad_inval_published_total).",
	}, []string{"kind"})
	dropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_inval_dropped_total",
		Help: "Messages this replica did not send, by reason: redis_down (Redis was degraded), publish_error (the publish failed) and queue_full (the burst exceeded the queue and became one resync). Each one is a gap the peers notice from the epoch or heal from the TTL; a rising rate means the other replicas serve stale caches until their TTL.",
	}, []string{"reason"})
	ignored = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_inval_ignored_total",
		Help: "Messages from other replicas this replica could not act on, by reason: unknown_key (an invalidation for a cache this release does not have, a newer release's), bad_scope, no_origin and oversized. A steady unknown_key rate during a rolling deploy is the other release's newer caches.",
	}, []string{"reason"})
	gapsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_inval_gap_total",
		Help: "Gaps in a peer's message numbering noticed on receipt: a message (or its heartbeat's epoch) skipped ahead, so one was lost. Each triggers a resync. Recommended alert: a sustained rate means the channel loses messages.",
	}, []string{})
	resyncsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_inval_resync_total",
		Help: "Times this replica dropped every replicated cache and re-checked its sockets, by reason: subscribe (the subscription was (re)established), health (Redis recovered), gap (a peer's epoch skipped) and peer (a peer asked: its queue overflowed).",
	}, []string{"reason"})
)

func init() {
	metrics.MustRegister(published, received, dropped, ignored, gapsTotal, resyncsTotal)
}
