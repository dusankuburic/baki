package audit

import (
	"context"
	"errors"
	"time"

	"go.uber.org/fx"

	"pad-analyzer/internal/lifecycle"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// orgBackfillLockKey serialises the org_id backfill across replicas; it differs from the other advisory keys
// ("bakiscan", "bakimigr", "bakiretn", "bakifenc").
const orgBackfillLockKey int64 = 0x62616b696175646f // "bakiaudo"

// orgBackfillTimeout bounds one run; stopping the app cancels it sooner.
const orgBackfillTimeout = 10 * time.Minute

// OrgBackfiller is a store that can fill audit_events.org_id on the rows written without one: an org
// resource's row gets its resource_id, a flow's the flow's organisation; other rows keep none. Batched,
// idempotent and resumable (Postgres implements it).
type OrgBackfiller interface {
	BackfillAuditOrgIDs(ctx context.Context) (int, error)
}

type globalLocker interface {
	TryGlobalLock(ctx context.Context, key int64) (release func(), acquired bool, err error)
}

// BackfillOrgIDs runs the org_id backfill once, under the store's cross-replica lock when it has one: a replica
// that finds the lock held skips the run, the holder does the work. It reports the rows filled and whether this
// call ran the backfill; a store without the capability (desktop, the filesystem backend) has nothing to fill.
func BackfillOrgIDs(ctx context.Context, store storageif.StorageBackend) (filled int, ran bool, err error) {
	bf, ok := store.(OrgBackfiller)
	if !ok {
		return 0, false, nil
	}
	if locker, ok := store.(globalLocker); ok {
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
func ScheduleOrgBackfill(lc fx.Lifecycle, store storageif.StorageBackend) {
	if _, ok := store.(OrgBackfiller); !ok {
		return
	}
	stopCtx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			orgBackfill.Go(func() {
				ctx, done := context.WithTimeout(stopCtx, orgBackfillTimeout)
				defer done()
				n, ran, err := BackfillOrgIDs(ctx, store)
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
