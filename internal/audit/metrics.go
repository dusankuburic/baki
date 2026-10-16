package audit

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"pad-analyzer/internal/metrics"
)

// The audit trail's metrics, registered from this package.
var (
	auditDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_audit_dropped_total",
			Help: "Audit events not persisted to the DB (pool full/closed) and diverted to the structured-log fallback sink, by reason.",
		},
		[]string{"reason"},
	)
	auditSpilledTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_audit_spilled_total",
		Help: "Audit events written to the on-disk spill queue, by reason: full (the in-memory pool was full) or db_unavailable (the database could not be reached after the write's retry; an event replayed into a continuing outage is spilled and counted again). A reaper drains them back into the DB pool when there is room and the database answers.",
	}, []string{"reason"})
	auditSpillReplayedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_audit_spill_replayed_total",
		Help: "Audit events drained from the on-disk spill queue and re-enqueued to the DB pool.",
	})
	auditSpillDroppedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_audit_spill_dropped_total",
		Help: "Audit events dropped because the on-disk spill queue was at its size cap — only happens under sustained overload beyond the spill capacity.",
	})
	auditSpillUndecodableTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_audit_spill_undecodable_total",
		Help: "Lines of the on-disk audit spill queue skipped on replay because they do not decode: the torn last line of a writer that died mid-append, or a damaged file. The event on that line is lost; the lines after it replay.",
	})
	auditSpillDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_audit_spill_depth",
		Help: "Audit events currently on the on-disk spill queue (overflow past the in-memory pool, or writes the database was unreachable for). Non-zero means the DB sink fell behind or is down; pair with pad_audit_spill_replayed_total to confirm recovery.",
	})
)

func init() {
	metrics.MustRegister(auditDroppedTotal, auditSpilledTotal, auditSpillReplayedTotal, auditSpillDroppedTotal,
		auditSpillUndecodableTotal, auditSpillDepth)
	for _, reason := range []string{spillReasonFull, spillReasonUnreachable} {
		auditSpilledTotal.WithLabelValues(reason)
	}
}

// The reasons of pad_audit_spilled_total.
const (
	spillReasonFull        = "full"
	spillReasonUnreachable = "db_unavailable"
)

// recordAuditDropped bumps the audit_dropped_total counter for an event that
// could not be persisted to the DB sink — "full"/"closed" when the enqueue
// buffer can't accept it, "write_failed" when a (retried) DB write fails. The
// event is diverted to the structured-log fallback sink; the counter surfaces
// when the DB sink can't keep up.
func recordAuditDropped(reason string) {
	auditDroppedTotal.WithLabelValues(reason).Inc()
}

// auditDroppedCount returns the current audit_dropped_total value for a reason.
// For tests.
func auditDroppedCount(reason string) float64 {
	m := &dto.Metric{}
	if err := auditDroppedTotal.WithLabelValues(reason).Write(m); err != nil {
		return 0
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// recordAuditSpilled bumps pad_audit_spilled_total — an event went to the
// on-disk spill queue, for reason (spillReasonFull, spillReasonUnreachable).
func recordAuditSpilled(reason string) {
	auditSpilledTotal.WithLabelValues(reason).Inc()
}

// recordAuditSpillUndecodable adds n to pad_audit_spill_undecodable_total — spill lines that do not decode were skipped.
func recordAuditSpillUndecodable(n int) {
	auditSpillUndecodableTotal.Add(float64(n))
}

// auditSpillUndecodableCount returns the current pad_audit_spill_undecodable_total value. For tests.
func auditSpillUndecodableCount() float64 {
	m := &dto.Metric{}
	if err := auditSpillUndecodableTotal.Write(m); err != nil || m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// auditSpilledReasonCount returns the current pad_audit_spilled_total value for reason. For tests.
func auditSpilledReasonCount(reason string) float64 {
	m := &dto.Metric{}
	if err := auditSpilledTotal.WithLabelValues(reason).Write(m); err != nil || m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// recordAuditSpillReplayed bumps pad_audit_spill_replayed_total — a spilled
// event was drained back into the DB pool.
func recordAuditSpillReplayed() {
	auditSpillReplayedTotal.Inc()
}

// recordAuditSpillDropped bumps pad_audit_spill_dropped_total — the spill
// queue was at capacity and could not absorb the event.
func recordAuditSpillDropped() {
	auditSpillDroppedTotal.Inc()
}

// auditSpillDroppedCount returns the current pad_audit_spill_dropped_total
// value. For tests.
func auditSpillDroppedCount() float64 {
	m := &dto.Metric{}
	if err := auditSpillDroppedTotal.Write(m); err != nil {
		return 0
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// auditSpilledCount returns the current pad_audit_spilled_total value over every reason.
// For tests.
func auditSpilledCount() float64 {
	return auditSpilledReasonCount(spillReasonFull) + auditSpilledReasonCount(spillReasonUnreachable)
}

// setAuditSpillDepth sets the on-disk audit spill queue depth gauge.
func setAuditSpillDepth(n int) {
	auditSpillDepth.Set(float64(n))
}

// auditSpillDepthCount returns the current pad_audit_spill_depth value.
// For tests.
func auditSpillDepthCount() float64 {
	m := &dto.Metric{}
	if err := auditSpillDepth.Write(m); err != nil {
		return 0
	}
	if m.Gauge == nil {
		return 0
	}
	return m.Gauge.GetValue()
}
