package app

import (
	"context"
	"time"

	"go.uber.org/fx"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// fieldEncryptionLockKey serializes the at-rest backfill of secret columns across replicas.
const fieldEncryptionLockKey int64 = 0x62616b6966656e63 // "bakifenc"

// fieldEncryptionJob is the backfill's identity across replicas: a replica that finds the lock held skips the run,
// the holder does the work. It keeps the key it shipped with, so a rolling deploy's two revisions exclude each other.
var fieldEncryptionJob = lifecycle.DeclareJob(lifecycle.JobDecl{
	Name:      "field_encryption_backfill",
	LegacyKey: fieldEncryptionLockKey,
	Schedule:  "once per boot, in the background (database with PAD_ENCRYPTION_KEY)",
})

// fieldEncryptionBackfillTimeout bounds one run; stopping the app cancels it sooner, and the next boot resumes.
const fieldEncryptionBackfillTimeout = 5 * time.Minute

// fieldEncryptionLockRetry is how soon a boot whose lock could not be asked for tries again, instead of at the next
// boot.
const fieldEncryptionLockRetry = 5 * time.Minute

// scheduleFieldEncryptionBackfill seals, once per boot across replicas, the rows written in plaintext before field
// encryption was on. A lock error retries in a few minutes, a lost lock cancels the run, and an interrupted run
// resumes on the next boot (the backfill is idempotent). Invoked after startServer, so its stop hook runs first and
// cancels the backfill: before, the shutdown drain of the background runners waited for it (up to its 5 minutes, cut
// off by the drain's budget).
func scheduleFieldEncryptionBackfill(lc fx.Lifecycle, cfg *config.Config, fe storagedb.FieldEncryptor, leaser lock.Leaser, backend storageif.StorageBackend) {
	if fe == nil || !keystoreKey(cfg.Auth).IsSet() {
		return
	}
	job := lifecycle.NewSingleton(lifecycle.Job{
		Spec:             fieldEncryptionJob,
		RunAtStart:       true,
		Timeout:          fieldEncryptionBackfillTimeout,
		RetryOnLockError: fieldEncryptionLockRetry,
		Run: func(ctx context.Context) error {
			n, err := fe.EncryptOrgChannelsAtRest(ctx)
			if n > 0 {
				logger.Info("field encryption backfill sealed plaintext org channels", "rows", n)
			}
			return err // a failure is logged by the job ("job run failed"); the next boot resumes
		},
	}, leaser, backend)
	lc.Append(fx.Hook{OnStart: func(context.Context) error {
		job.Start()
		return nil
	}, OnStop: func(context.Context) error {
		job.Stop()
		return nil
	}})
}
