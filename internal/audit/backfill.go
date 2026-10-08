package audit

import (
	"context"
	"errors"
	"time"

	"go.uber.org/fx"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
	"pad-core/logger"
)

// orgBackfillLockKey serialises the org_id backfill across replicas; it differs from the other advisory keys
// ("bakiscan", "bakimigr", "bakiretn", "bakifenc").
const orgBackfillLockKey int64 = 0x62616b696175646f // "bakiaudo"

// orgBackfillTimeout bounds one run; stopping the app cancels it sooner.
const orgBackfillTimeout = 10 * time.Minute

// OrgBackfiller fills audit_events.org_id on the rows written without one: an org resource's row gets its
// resource_id, a flow's the flow's organisation; other rows keep none. Batched, idempotent and resumable. The
// database backend provides it; nil without a database, where there is nothing to fill.
type OrgBackfiller interface {
	BackfillAuditOrgIDs(ctx context.Context) (int, error)
}

// BackfillOrgIDs runs the org_id backfill once, under the cross-replica lock when there is one: a replica that
// finds the lock held skips the run, the holder does the work. It reports the rows filled and whether this call
// ran the backfill; without a backfiller there is nothing to fill.
func BackfillOrgIDs(ctx context.Context, bf OrgBackfiller, locker lock.GlobalLocker) (filled int, ran bool, err error) {
	if bf == nil {
		return 0, false, nil
	}
	if locker != nil {
		release, acquired, err := locker.TryGlobalLock(ctx, orgBackfillLockKey)
		if err != nil || !acquired {
			return 0, false, err
		}
		defer release()
	}
	filled, err = bf.BackfillAuditOrgIDs(ctx)
	return filled, true, err
}

// orgBackfill runs the boot-time backfill off the boot path; shutdown drains it.
var orgBackfill = lifecycle.NewBounded("audit_org_backfill", 1)

// ScheduleOrgBackfill runs BackfillOrgIDs once per boot after the migrations, in the background. Invoke it
// after the server is set up: its stop hook then runs first and cancels the run, so the shutdown drain of the
// background runners does not wait for it. An interrupted run resumes on the next boot.
func ScheduleOrgBackfill(lc fx.Lifecycle, bf OrgBackfiller, locker lock.GlobalLocker) {
	if bf == nil {
		return
	}
	stopCtx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			orgBackfill.Go(func() {
				ctx, done := context.WithTimeout(stopCtx, orgBackfillTimeout)
				defer done()
				n, ran, err := BackfillOrgIDs(ctx, bf, locker)
				switch {
				case err != nil && !ran:
					logger.Warn("audit org_id backfill skipped: lock unavailable", "error", err)
				case errors.Is(err, context.Canceled):
					logger.Info("audit org_id backfill interrupted by shutdown; it resumes on the next boot", "filled", n)
				case err != nil:
					logger.Error("audit org_id backfill stopped; it resumes on the next boot", "filled", n, "error", err)
				case ran && n > 0:
					logger.Info("audit org_id backfill filled audit rows", "rows", n)
				}
			})
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			return nil
		},
	})
}
