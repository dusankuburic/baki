package eventbus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// The bus's metrics, registered from this package.
var (
	eventsPublishedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_events_published_total",
			Help: "Domain events published on the in-process event bus, by event.",
		},
		[]string{"event"},
	)
	eventSubscriberFailedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_event_subscriber_failed_total",
			Help: "Event deliveries a subscriber did not complete, by event, subscriber and reason (panic, timeout, dropped: the async runner was saturated or shutting down).",
		},
		[]string{"event", "subscriber", "reason"},
	)
	eventSubscriberDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pad_event_subscriber_duration_seconds",
			Help:    "Run time of one event delivery to one subscriber, by event, subscriber and tier (sync, async).",
			Buckets: []float64{0.0001, 0.001, 0.01, 0.05, 0.25, 1, 5, 30},
		},
		[]string{"event", "subscriber", "tier"},
	)
)

func init() {
	metrics.MustRegister(eventsPublishedTotal, eventSubscriberFailedTotal, eventSubscriberDuration)
}

// recordEventPublished counts one domain event published on the event bus.
func recordEventPublished(event string) {
	eventsPublishedTotal.WithLabelValues(event).Inc()
}

// recordEventDelivery records one delivery of event to subscriber on tier ("sync" or "async"); failure is "" for a
// delivery that completed, else its reason ("panic", "timeout", "dropped").
func recordEventDelivery(event, subscriber, tier string, d time.Duration, failure string) {
	if failure != "" {
		eventSubscriberFailedTotal.WithLabelValues(event, subscriber, failure).Inc()
		if failure == "dropped" {
			return
		}
	}
	eventSubscriberDuration.WithLabelValues(event, subscriber, tier).Observe(d.Seconds())
}
