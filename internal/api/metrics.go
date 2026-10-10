package api

import (
	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// The handlers' own metrics, registered from this package. The HTTP layer's are in internal/api/middleware.
var (
	sseClientsConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sse_clients_connected",
		Help: "Currently connected Server-Sent Events subscribers.",
	})
	authOps = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_auth_operations_total",
			Help: "Number of auth operations by type",
		},
		[]string{"op"},
	)
)

func init() { metrics.MustRegister(sseClientsConnected, authOps) }

// SSE-subscriber gauges (called from the SSE handler).
func sseClientStart() { sseClientsConnected.Inc() }
func sseClientEnd()   { sseClientsConnected.Dec() }

// recordAuthOp increments the auth-operations counter for the given op type
// (e.g. "login", "register", "logout", "refresh").
func recordAuthOp(op string) {
	authOps.WithLabelValues(op).Inc()
}
