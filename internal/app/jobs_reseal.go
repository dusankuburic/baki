package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/fx"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// Key rotation (ADR 0005): with PAD_ENCRYPTION_KEY_PREVIOUS set, every boot moves the values a previous key sealed
// (the secret columns and the keystore) to PAD_ENCRYPTION_KEY, on one replica at a time, and logs what it found.

// secretResealLockKey runs one re-seal pass at a time across replicas; it differs from the other advisory keys
// ("bakiscan", "bakimigr", "bakiretn", "bakifenc", "bakiaudo", "bakiblck").
const secretResealLockKey int64 = 0x62616b696b726f74 // "bakikrot"

// secretResealJob is the re-seal's identity across replicas: a replica that finds the lock held skips the pass, the
// holder does the work. It keeps the key it shipped with, so a rolling deploy's two revisions exclude each other.
var secretResealJob = lifecycle.DeclareJob(lifecycle.JobDecl{
	Name:      "secret_reseal",
	LegacyKey: secretResealLockKey,
	Schedule:  "once per boot, in the background, while a key rotation is under way (database with PAD_ENCRYPTION_KEY_PREVIOUS)",
})

// secretResealTimeout bounds one pass; stopping the app cancels it sooner, and the next boot resumes.
const secretResealTimeout = 30 * time.Minute

// secretResealLockRetry is how soon a boot whose lock could not be asked for tries again, instead of at the next
// boot.
const secretResealLockRetry = 5 * time.Minute

// resealFunc is one store's re-seal pass.
type resealFunc func(context.Context) (storagedb.ResealReport, error)

// scheduleSecretReseal runs the re-seal pass once per boot, in the background, when a rotation is under way
// (previous keys configured); without a database nothing is sealed. A lock error retries in a few minutes and a lost
// lock cancels the pass. Invoked after startServer, so its stop hook runs first and cancels the pass, and the
// shutdown drain of the background runners does not wait for it.
func scheduleSecretReseal(lc fx.Lifecycle, cfg *config.Config, fe storagedb.FieldEncryptor, ks storagedb.KeyResealer, leaser lock.Leaser, backend storageif.StorageBackend) {
	if len(keystorePreviousKeys(cfg.Auth)) == 0 {
		return
	}
	var passes []resealFunc
	if fe != nil {
		passes = append(passes, fe.ResealFields)
	}
	if ks != nil {
		passes = append(passes, ks.ResealKeys)
	}
	if len(passes) == 0 {
		return
	}
	job := lifecycle.NewSingleton(resealJob(passes, logSecretReseal), leaser, backend)
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			job.Start()
			return nil
		},
		OnStop: func(context.Context) error {
			job.Stop()
			return nil
		},
	})
}

// resealJob is the re-seal as a job: the passes in turn, at boot, on one replica at a time (a replica that finds the
// lock held skips; the holder does the work). report gets what the passes found, also when one stopped on an error;
// a skipped attempt is the job's to log.
func resealJob(passes []resealFunc, report func(storagedb.ResealReport, error)) lifecycle.Job {
	return lifecycle.Job{
		Spec:             secretResealJob,
		RunAtStart:       true,
		Timeout:          secretResealTimeout,
		RetryOnLockError: secretResealLockRetry,
		Run: func(ctx context.Context) error {
			var total storagedb.ResealReport
			for _, pass := range passes {
				rep, err := pass(ctx)
				total.Add(rep)
				if err != nil {
					report(total, err)
					return err
				}
			}
			report(total, nil)
			return nil
		},
	}
}

// logSecretReseal reports a pass: the counts always, and each value no configured key opens by table, column and
// row id (never its content or a key). The operator removes PAD_ENCRYPTION_KEY_PREVIOUS once a pass that ran after
// every replica took the new configuration reports nothing remaining and nothing unreadable. Why a pass was
// interrupted (shutdown, or its lock was lost) is the job's line beside it.
func logSecretReseal(rep storagedb.ResealReport, err error) {
	counts := []any{"checked", rep.Checked, "resealed", rep.Resealed, "remaining", rep.Remaining, "unreadable", rep.Unreadable}
	switch {
	case errors.Is(err, context.Canceled):
		logger.Info("secret re-seal interrupted; it resumes on the next boot", counts...)
	case err != nil:
		logger.Error("secret re-seal stopped; it resumes on the next boot", append(counts, "error", err)...)
	case rep.Remaining == 0 && rep.Unreadable == 0:
		logger.Info("secret re-seal complete: every sealed value opens with PAD_ENCRYPTION_KEY", counts...)
	default:
		logger.Warn("secret re-seal finished with values still on a previous key or unreadable; keep PAD_ENCRYPTION_KEY_PREVIOUS", counts...)
	}
	if rep.Unreadable > 0 {
		refs := make([]string, len(rep.UnreadableRefs))
		for i, r := range rep.UnreadableRefs {
			refs[i] = fmt.Sprintf("%s.%s[%s]", r.Table, r.Column, r.RowID)
		}
		logger.Warn("secret re-seal: values no configured key opens were left as stored", "unreadable", rep.Unreadable, "values", refs)
	}
}
