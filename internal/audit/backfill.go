package audit

import (
	"context"
	"errors"
	"time"

	"go.uber.org/fx"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// orgBackfillLockKey serialises the org_id backfill across replicas; it differs from the other advisory keys
// ("bakiscan", "bakimigr", "bakiretn", "bakifenc").
const orgBackfillLockKey int64 = 0x62616b696175646f // "bakiaudo"

// orgBackfillJob is the backfill's identity across replicas: a replica that finds the lock held skips the run, the
// holder does the work.
var orgBackfillJob = lifecycle.DeclareJob(lifecycle.JobDecl{
	Name:      "audit_org_backfill",
	LegacyKey: orgBackfillLockKey,
	Schedule:  "once per boot, in the background (server store)",
})

// orgBackfillTimeout bounds one run; stopping the app cancels it sooner.
const orgBackfillTimeout = 10 * time.Minute

// orgBackfillRetry is how soon a boot whose lock could not be asked for tries again, instead of at the next boot.
const orgBackfillRetry = 5 * time.Minute

// OrgBackfiller fills audit_events.org_id on the rows written without one: an org resource's row gets its
// resource_id, a flow's the flow's organisation; other rows keep none. Batched, idempotent and resumable. The
// database backend provides it; nil without a database, where there is nothing to fill.
type OrgBackfiller interface {
	BackfillAuditOrgIDs(ctx context.Context) (int, error)
}

// orgBackfill is the job's work; ran and filled report a run to BackfillOrgIDs (nil: logged only).
func orgBackfill(bf OrgBackfiller, filled *int, ran *bool) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := bf.BackfillAuditOrgIDs(ctx)
		if ran != nil {
			*ran, *filled = true, n
		}
		if err == nil && n > 0 {
			logger.Info("audit org_id backfill filled audit rows", "rows", n)
		}
		return err
	}
}

// BackfillOrgIDs runs the org_id backfill once, now, under the cross-replica lock when there is one: a replica that
// finds the lock held skips the run, the holder does the work, and a lock that cannot be asked for skips it with
// the error. It reports the rows filled and whether this call ran the backfill; without a backfiller there is
// nothing to fill.
func BackfillOrgIDs(ctx context.Context, bf OrgBackfiller, locker lock.GlobalLocker) (filled int, ran bool, err error) {
	if bf == nil {
		return 0, false, nil
	}
	job := lifecycle.NewSingleton(lifecycle.Job{Spec: orgBackfillJob, Run: orgBackfill(bf, &filled, &ran)},
		lock.AsLeaser(locker), nil)
	defer job.Stop()
	err = job.RunOnce(ctx)
	if errors.Is(err, lifecycle.ErrJobHeld) {
		return 0, false, nil
	}
	return filled, ran, err
}

// BackfillDeps is what ScheduleOrgBackfill takes from the app graph; nil without a database. Leaser, when the app
// provides one, replaces Locker: a run whose lock is lost is cancelled. Store keeps the job's run state.
type BackfillDeps struct {
	fx.In
	Lifecycle  fx.Lifecycle
	Backfiller OrgBackfiller            `optional:"true"`
	Locker     lock.GlobalLocker        `optional:"true"`
	Leaser     lock.Leaser              `optional:"true"`
	Store      storageif.StorageBackend `optional:"true"`
}

// ScheduleOrgBackfill runs the backfill job once per boot after the migrations, in the background (retrying a lock
// that could not be asked for). Invoke it after the server is set up: its stop hook then runs first and cancels the
// run, so the shutdown drain of the background runners does not wait for it. An interrupted run resumes on the next
// boot.
func ScheduleOrgBackfill(d BackfillDeps) {
	if d.Backfiller == nil {
		return
	}
	leaser := d.Leaser
	if leaser == nil {
		leaser = lock.AsLeaser(d.Locker)
	}
	var state lifecycle.JobState
	if d.Store != nil {
		state = d.Store
	}
	job := lifecycle.NewSingleton(lifecycle.Job{
		Spec:             orgBackfillJob,
		RunAtStart:       true,
		Timeout:          orgBackfillTimeout,
		RetryOnLockError: orgBackfillRetry,
		Run:              orgBackfill(d.Backfiller, nil, nil),
	}, leaser, state)
	d.Lifecycle.Append(fx.Hook{
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
