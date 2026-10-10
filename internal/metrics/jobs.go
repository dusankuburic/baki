package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Background job metrics (lifecycle.Singleton, ADR 0023). The job label is a declared job name
// (lifecycle.DeclareJob takes a literal at package level), a closed set; outcome and reason are the constants below.
// Registered from this file so the central list in metrics.go does not grow.
var (
	jobRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_job_runs_total",
			Help: "Runs of each background job on this replica, by outcome: ok, failed, lease_lost (the job's cross-replica lock was lost while it ran; its context was cancelled, or the unlock found the lock gone), panic.",
		},
		[]string{"job", "outcome"},
	)
	jobSkippedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_job_skipped_total",
			Help: "Attempts of each background job this replica skipped, by reason: held (another replica, or a run of this one, holds the job's lock: normal on every replica but the holder) or lock_error (the lock could not be asked for; the job fails closed and retries).",
		},
		[]string{"job", "reason"},
	)
	jobDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pad_job_duration_seconds",
			Help:    "Run time of each background job on this replica, whatever its outcome.",
			Buckets: []float64{0.1, 1, 10, 60, 300, 900, 1800, 3600, 7200},
		},
		[]string{"job"},
	)
	jobLastSuccess = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pad_job_last_success_timestamp_seconds",
			Help: "Unix time of the last successful run of each background job on any replica, as this replica last saw it (its own run, or the persisted job state read when it skipped).",
		},
		[]string{"job"},
	)
	jobOverdue = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pad_job_overdue",
			Help: "1 while a background job has had no successful run on any replica for longer than its overdue threshold (default twice its interval), as this replica saw it at its last attempt; 0 otherwise. Pair with the \"job overdue\" WARN log line.",
		},
		[]string{"job"},
	)
	advisoryLeaseLostTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_advisory_lease_lost_total",
		Help: "Held advisory-lock leases whose heartbeat found the lock gone or the pinned session dead (the database dropped the lock; another replica may take it).",
	})
	advisoryUnlockNotHeldTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_advisory_unlock_not_held_total",
		Help: "Advisory unlocks that reported the lock was not held by the session: the lock was lost while held, or a transaction-mode pooler ran the unlock on another server session (unsupported).",
	})
)

// Outcomes of pad_job_runs_total.
const (
	JobOutcomeOK        = "ok"
	JobOutcomeFailed    = "failed"
	JobOutcomeLeaseLost = "lease_lost"
	JobOutcomePanic     = "panic"
)

// Reasons of pad_job_skipped_total.
const (
	JobSkipHeld      = "held"
	JobSkipLockError = "lock_error"
)

func init() {
	registry.MustRegister(
		jobRunsTotal,
		jobSkippedTotal,
		jobDuration,
		jobLastSuccess,
		jobOverdue,
		advisoryLeaseLostTotal,
		advisoryUnlockNotHeldTotal,
	)
}

// RecordJobRun counts one run of job with its outcome and run time.
func RecordJobRun(job, outcome string, took time.Duration) {
	jobRunsTotal.WithLabelValues(job, outcome).Inc()
	jobDuration.WithLabelValues(job).Observe(took.Seconds())
}

// RecordJobSkipped counts one skipped attempt of job.
func RecordJobSkipped(job, reason string) {
	jobSkippedTotal.WithLabelValues(job, reason).Inc()
}

// SetJobLastSuccess records the time of job's last successful run on any replica.
func SetJobLastSuccess(job string, at time.Time) {
	jobLastSuccess.WithLabelValues(job).Set(float64(at.UnixNano()) / 1e9)
}

// SetJobOverdue sets job's overdue gauge.
func SetJobOverdue(job string, overdue bool) {
	v := 0.0
	if overdue {
		v = 1
	}
	jobOverdue.WithLabelValues(job).Set(v)
}

// RecordAdvisoryLeaseLost counts a lease whose heartbeat found its lock gone.
func RecordAdvisoryLeaseLost() { advisoryLeaseLostTotal.Inc() }

// RecordAdvisoryUnlockNotHeld counts an unlock that reported the lock not held.
func RecordAdvisoryUnlockNotHeld() { advisoryUnlockNotHeldTotal.Inc() }

// AdvisoryUnlockNotHeldCount reads pad_advisory_unlock_not_held_total, for tests outside this package.
func AdvisoryUnlockNotHeldCount() float64 { return counterValue(advisoryUnlockNotHeldTotal) }

// AdvisoryLeaseLostCount reads pad_advisory_lease_lost_total, for tests outside this package.
func AdvisoryLeaseLostCount() float64 { return counterValue(advisoryLeaseLostTotal) }

// JobRunCount reads pad_job_runs_total{job,outcome}, for tests outside this package.
func JobRunCount(job, outcome string) float64 {
	return counterValue(jobRunsTotal.WithLabelValues(job, outcome))
}

// JobSkippedCount reads pad_job_skipped_total{job,reason}, for tests outside this package.
func JobSkippedCount(job, reason string) float64 {
	return counterValue(jobSkippedTotal.WithLabelValues(job, reason))
}

// JobOverdueValue reads pad_job_overdue{job}, for tests outside this package.
func JobOverdueValue(job string) float64 {
	m := &dto.Metric{}
	if err := jobOverdue.WithLabelValues(job).Write(m); err != nil || m.Gauge == nil {
		return 0
	}
	return m.Gauge.GetValue()
}
