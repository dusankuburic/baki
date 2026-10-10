package app

import (
	"context"
	"time"

	"go.uber.org/fx"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/lifecycle"
	padmetrics "pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/lock"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// initRetentionPurge periodically purges expired tokens/invites and aged-out
// audit rows (GDPR data-minimisation); disabled with no interval or backend.
// An immediate sweep runs on start so a fresh instance clears stale rows.
func initRetentionPurge(lc fx.Lifecycle, cfg *config.Config, backend storageif.StorageBackend, leaser lock.Leaser) {
	interval := cfg.Governance.RetentionPurgeInterval.Std()
	// With accounts on the server store (cloud, and local+db with PAD_AUTH_ENABLED): what it purges (tokens,
	// invitations, CI credentials, aged audit rows) exists there. Desktop's purge is a no-op that would log a
	// "purge complete" line of zeros every day, and local+db without accounts keeps its history.
	if interval <= 0 || backend == nil || !capability.Of(cfg).Has(capability.ServerStore, capability.Accounts) {
		return
	}
	// The Singleton's Stop cancels the in-flight sweep AND waits (up to 5s) for
	// it, so storage's OnStop, which runs after this one, cannot close the pool
	// under a sweep still deleting (the server's drain stops it even sooner).
	job := lifecycle.NewSingleton(retentionJob(backend, interval, cfg.Governance.AuditRetentionDays), leaser, backend)
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error { job.Start(); return nil },
		OnStop:  func(context.Context) error { job.Stop(); return nil },
	})
}

// retentionLockKey namespaces the retention sweep's advisory lock: every replica
// must use the same key, and it must differ from the scanner's ("bakiscan") and
// the migration runner's ("bakimigr").
const retentionLockKey int64 = 0x62616b697265746e // "bakiretn"

// retentionPurgeJob is the purge's identity across replicas: one sweep at a time (without it N replicas raced the
// same DELETEs). It keeps the key it shipped with, so the old and the new revision of a rolling deploy exclude each
// other.
var retentionPurgeJob = lifecycle.DeclareJob(lifecycle.JobDecl{
	Name:      "retention_purge",
	LegacyKey: retentionLockKey,
	Schedule:  "every PAD_RETENTION_PURGE_INTERVAL and once at boot (server store with accounts)",
})

// retentionSweepTimeout bounds one whole sweep; each batched statement is
// still cut off by statement_timeout — this is only the ceiling on the sum.
const retentionSweepTimeout = 10 * time.Minute

// retentionLockRetry is how soon a sweep skipped for a lock error (a database blip) is tried again, instead of a
// whole interval later.
const retentionLockRetry = 5 * time.Minute

// retentionJob is the purge as a job: at boot and every interval, on one replica at a time. A lock error skips the
// sweep (fail closed: sweeping without the lock let every replica purge at once) and retries it soon. A purge that
// ran nowhere for twice its interval is overdue (`job overdue`, pad_job_overdue): personal data past its retention
// is being kept.
func retentionJob(backend storageif.StorageBackend, interval time.Duration, auditRetentionDays int) lifecycle.Job {
	return lifecycle.Job{
		Spec:             retentionPurgeJob,
		Every:            interval,
		RunAtStart:       true,
		RetryOnLockError: retentionLockRetry,
		OverdueAfter:     2 * interval,
		Run: func(ctx context.Context) error {
			return purgeExpiredData(ctx, backend, auditRetentionDays)
		},
	}
}

// purgeExpiredData is one retention sweep, bounded by retentionSweepTimeout however it was started. A partial purge
// is an error: the job is not counted a success until a sweep finishes.
func purgeExpiredData(ctx context.Context, backend storageif.StorageBackend, auditRetentionDays int) error {
	ctx, cancel := context.WithTimeout(ctx, retentionSweepTimeout)
	defer cancel()
	res, err := backend.PurgeExpiredData(ctx, auditRetentionDays)
	if err != nil && res == nil {
		return err // logged by the job ("job run failed")
	}
	if err != nil {
		// Partial: the purge continues past a table whose batches failed
		// (timed out) and reports what the other tables removed; the next
		// tick resumes. Log the counts, not just the failure.
		logger.Warn("retention purge incomplete", "err", err,
			"audit_events", res.AuditEvents, "flow_analysis_history", res.FlowAnalysisHistory,
			"usage_metrics", res.UsageMetrics, "token_blacklist", res.TokenBlacklist,
			"gov_alerts", res.GovAlerts, "idempotency_keys", res.IdempotencyKeys, "ci_deliveries", res.CIDeliveries)
		padmetrics.RecordBackgroundLoopTick("retention_purge")
		return err
	}
	logger.Info("retention purge complete",
		"refresh_tokens", res.RefreshTokens, "api_tokens", res.APITokens,
		"user_tokens", res.UserTokens, "org_invites", res.OrgInvites,
		"audit_events", res.AuditEvents,
		"flow_analysis_history", res.FlowAnalysisHistory,
		"usage_metrics", res.UsageMetrics, "token_blacklist", res.TokenBlacklist,
		"share_tokens", res.ShareTokens, "gov_alerts", res.GovAlerts,
		"idempotency_keys", res.IdempotencyKeys, "ci_credentials", res.CICredentials,
		"ci_deliveries", res.CIDeliveries)
	padmetrics.RecordBackgroundLoopTick("retention_purge")
	return nil
}
