package audit

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/testutil"
)

// backfillStore is a store with the backfill and, optionally, a cross-replica lock.
type backfillStore struct {
	*testutil.FakeBackend
	calls    atomic.Int32
	filled   int
	block    bool // wait for the context instead of returning
	released atomic.Int32
}

func (s *backfillStore) BackfillAuditOrgIDs(ctx context.Context) (int, error) {
	s.calls.Add(1)
	if s.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return s.filled, nil
}

type lockedBackfillStore struct {
	*backfillStore
	acquired bool
	err      error
}

func (s *lockedBackfillStore) TryGlobalLock(context.Context, int64) (func(), bool, error) {
	if s.err != nil || !s.acquired {
		return nil, false, s.err
	}
	return func() { s.released.Add(1) }, true, nil
}

func TestBackfillOrgIDs_RunsUnderTheLock(t *testing.T) {
	ctx := context.Background()

	if _, ran, err := BackfillOrgIDs(ctx, testutil.NewFakeBackend()); ran || err != nil {
		t.Errorf("a store without the backfill: ran %v err %v", ran, err)
	}
	if _, ran, err := BackfillOrgIDs(ctx, nil); ran || err != nil {
		t.Errorf("desktop (no store): ran %v err %v", ran, err)
	}

	unlocked := &backfillStore{FakeBackend: testutil.NewFakeBackend(), filled: 3}
	if n, ran, err := BackfillOrgIDs(ctx, unlocked); n != 3 || !ran || err != nil {
		t.Errorf("a store without a lock: filled %d ran %v err %v, want 3 true nil", n, ran, err)
	}

	held := &lockedBackfillStore{backfillStore: &backfillStore{FakeBackend: testutil.NewFakeBackend()}}
	if _, ran, err := BackfillOrgIDs(ctx, held); ran || err != nil || held.calls.Load() != 0 {
		t.Errorf("lock held by another replica: ran %v err %v calls %d, want a skip", ran, err, held.calls.Load())
	}

	broken := &lockedBackfillStore{backfillStore: &backfillStore{FakeBackend: testutil.NewFakeBackend()}, err: errors.New("conn refused")}
	if _, ran, err := BackfillOrgIDs(ctx, broken); ran || err == nil || broken.calls.Load() != 0 {
		t.Errorf("lock unavailable: ran %v err %v calls %d, want a skip with the error", ran, err, broken.calls.Load())
	}

	mine := &lockedBackfillStore{backfillStore: &backfillStore{FakeBackend: testutil.NewFakeBackend(), filled: 5}, acquired: true}
	if n, ran, err := BackfillOrgIDs(ctx, mine); n != 5 || !ran || err != nil || mine.released.Load() != 1 {
		t.Errorf("lock acquired: filled %d ran %v err %v released %d, want 5 true nil 1", n, ran, err, mine.released.Load())
	}
}

// ScheduleOrgBackfill runs the backfill once when the app starts, and stopping the app cancels a run still
// going, so the shutdown drain does not wait for it.
func TestScheduleOrgBackfill_RunsOnStartAndStopsWithTheApp(t *testing.T) {
	store := &lockedBackfillStore{backfillStore: &backfillStore{FakeBackend: testutil.NewFakeBackend(), block: true}, acquired: true}
	lc := fxtest.NewLifecycle(t)
	ScheduleOrgBackfill(lc, store)
	lc.RequireStart()
	waitFor(t, "the backfill to start", func() bool { return store.calls.Load() == 1 })
	lc.RequireStop()
	waitFor(t, "the cancelled run to release its lock", func() bool { return store.released.Load() == 1 })

	none := fxtest.NewLifecycle(t)
	ScheduleOrgBackfill(none, testutil.NewFakeBackend()) // nothing to fill: no hook
	none.RequireStart()
	none.RequireStop()
}
