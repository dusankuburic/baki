package redisx

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// MsgsCounterForTest lets the external command tests read pad_redis_msgs_total.
func MsgsCounterForTest(kind, result string) prometheus.Counter {
	return msgsTotal.WithLabelValues(kind, result)
}

// BusRejectedForTest reads pad_redis_bus_rejected_total{reason}.
func BusRejectedForTest(reason string) prometheus.Counter { return busRejected.WithLabelValues(reason) }

// SetNowForTest replaces the clock the bus stamps and judges frames with.
func (b *CommandBus) SetNowForTest(now func() time.Time) { b.now = now }
