package audit

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/testutil"
)

// backfillStore is a backfiller that counts its runs.
type backfillStore struct {
	calls  atomic.Int32
	filled int
	block  bool // wait for the context instead of returning
}

func (s *backfillStore) BackfillAuditOrgIDs(ctx context.Context) (int, error) {
	s.calls.Add(1)
	if s.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return s.filled, nil
}

// testLocker is a cross-replica lock that is free (acquired), held by another replica, or unavailable (err).
type testLocker struct {
	acquired bool
	err      error
	released atomic.Int32
}

func (l *testLocker) TryGlobalLock(context.Context, int64) (func(), bool, error) {
	if l.err != nil || !l.acquired {
		return nil, false, l.err
	}
	return func() { l.released.Add(1) }, true, nil
}

func TestBackfillOrgIDs_RunsUnderTheLock(t *testing.T) {
	ctx := context.Background()

	if _, ran, err := BackfillOrgIDs(ctx, nil, nil); ran || err != nil {
		t.Errorf("desktop (no database): ran %v err %v", ran, err)
	}

	unlocked := &backfillStore{filled: 3}
	if n, ran, err := BackfillOrgIDs(ctx, unlocked, nil); n != 3 || !ran || err != nil {
		t.Errorf("without a lock: filled %d ran %v err %v, want 3 true nil", n, ran, err)
	}

	held := &backfillStore{}
	if _, ran, err := BackfillOrgIDs(ctx, held, &testLocker{}); ran || err != nil || held.calls.Load() != 0 {
		t.Errorf("lock held by another replica: ran %v err %v calls %d, want a skip", ran, err, held.calls.Load())
	}

	broken := &backfillStore{}
	if _, ran, err := BackfillOrgIDs(ctx, broken, &testLocker{err: errors.New("conn refused")}); ran || err == nil || broken.calls.Load() != 0 {
		t.Errorf("lock unavailable: ran %v err %v calls %d, want a skip with the error", ran, err, broken.calls.Load())
	}

	mine, lock := &backfillStore{filled: 5}, &testLocker{acquired: true}
	if n, ran, err := BackfillOrgIDs(ctx, mine, lock); n != 5 || !ran || err != nil || lock.released.Load() != 1 {
		t.Errorf("lock acquired: filled %d ran %v err %v released %d, want 5 true nil 1", n, ran, err, lock.released.Load())
	}
}

// ScheduleOrgBackfill runs the backfill once when the app starts, and stopping the app cancels a run still
// going, so the shutdown drain does not wait for it.
func TestScheduleOrgBackfill_RunsOnStartAndStopsWithTheApp(t *testing.T) {
	store, lock := &backfillStore{block: true}, &testLocker{acquired: true}
	lc := fxtest.NewLifecycle(t)
	ScheduleOrgBackfill(BackfillDeps{Lifecycle: lc, Backfiller: store, Locker: lock})
	lc.RequireStart()
	waitFor(t, "the backfill to start", func() bool { return store.calls.Load() == 1 })
	lc.RequireStop()
	waitFor(t, "the cancelled run to release its lock", func() bool { return lock.released.Load() == 1 })

	none := fxtest.NewLifecycle(t)
	ScheduleOrgBackfill(BackfillDeps{Lifecycle: none}) // no database: nothing to fill, no hook
	none.RequireStart()
	none.RequireStop()
}

// TestScheduleOrgBackfill_RecordsTheJobState: the boot's run is recorded in the job state the backend keeps, so every
// replica can tell when the backfill last succeeded anywhere.
func TestScheduleOrgBackfill_RecordsTheJobState(t *testing.T) {
	store, jobs := &backfillStore{filled: 2}, testutil.NewFakeBackend()
	lc := fxtest.NewLifecycle(t)
	ScheduleOrgBackfill(BackfillDeps{Lifecycle: lc, Backfiller: store, Store: jobs})
	lc.RequireStart()
	waitFor(t, "the backfill's recorded success", func() bool {
		run, err := jobs.GetJobRun(context.Background(), "audit_org_backfill")
		return err == nil && !run.LastSuccessAt.IsZero() && run.Holder != ""
	})
	lc.RequireStop()
	if got := store.calls.Load(); got != 1 {
		t.Errorf("the once-per-boot backfill ran %d times, want 1", got)
	}
}
