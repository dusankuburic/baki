package websocket

import (
	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// The WebSocket hub's metrics, registered from this package.
var (
	wsConnectionsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_ws_connections_active",
		Help: "Number of active WebSocket connections",
	})
	wsInboundRefusedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ws_inbound_refused_total",
			Help: "WebSocket frames dropped at ingress, by event type (a declared type, or \"other\") and reason: edit (needs editor rank and the write scope), unknown (not a client-sendable type), invalid (malformed).",
		},
		[]string{"type", "reason"},
	)
)

func init() { metrics.MustRegister(wsConnectionsActive, wsInboundRefusedTotal) }

// recordWSConnectionChange adjusts the active-WebSocket-connections gauge by
// delta (+1 on join, -1 on leave).
func recordWSConnectionChange(delta int64) {
	wsConnectionsActive.Add(float64(delta))
}

// recordWSInboundRefused counts one inbound WebSocket frame dropped at
// ingress. eventType must be a declared event type or "other"
// (refusedTypeLabel bounds it), never the client's raw text; reason is edit,
// unknown or invalid.
func recordWSInboundRefused(eventType, reason string) {
	wsInboundRefusedTotal.WithLabelValues(eventType, reason).Inc()
}
