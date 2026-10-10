package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
	"pad-analyzer/internal/platform/secret"
	storagedb "pad-analyzer/internal/storage/database"
)

// resealLocker is a cross-replica lock that is free (acquired), held by another replica, or unavailable (err).
type resealLocker struct {
	acquired bool
	err      error
	keys     []int64
	released atomic.Int32
}

func (l *resealLocker) TryGlobalLock(_ context.Context, key int64) (func(), bool, error) {
	l.keys = append(l.keys, key)
	if l.err != nil || !l.acquired {
		return nil, false, l.err
	}
	return func() { l.released.Add(1) }, true, nil
}

// TryLease takes the same lock, as a lease that cannot report losing it.
func (l *resealLocker) TryLease(ctx context.Context, key int64) (lock.Lease, bool, error) {
	return lock.AsLeaser(l).TryLease(ctx, key)
}

// runSecretReseal is one attempt of the re-seal job as one replica's boot makes it: the replica's own Singleton over
// the lock the replicas share (none: it runs alone). It returns what the passes found, also when one stops on an
// error, and whether this attempt ran them (a replica that finds the lock held skips: the holder does the work).
func runSecretReseal(ctx context.Context, leaser lock.Leaser, passes ...resealFunc) (storagedb.ResealReport, bool, error) {
	var found storagedb.ResealReport
	ran := false
	job := lifecycle.NewSingleton(resealJob(passes, func(rep storagedb.ResealReport, _ error) { found, ran = rep, true }), leaser, nil)
	defer job.Stop()
	err := job.RunOnce(ctx)
	if errors.Is(err, lifecycle.ErrJobHeld) {
		err = nil
	}
	return found, ran, err
}

// fakeResealer is the field encryptor and the keystore's re-seal pass, counting runs.
type fakeResealer struct {
	calls  atomic.Int32
	report storagedb.ResealReport
	err    error
	block  bool // wait for the context instead of returning
}

func (f *fakeResealer) run(ctx context.Context) (storagedb.ResealReport, error) {
	f.calls.Add(1)
	if f.block {
		<-ctx.Done()
		return storagedb.ResealReport{}, ctx.Err()
	}
	return f.report, f.err
}

func (f *fakeResealer) EnableFieldEncryption([]byte, ...[]byte) error         { return nil }
func (f *fakeResealer) EncryptOrgChannelsAtRest(context.Context) (int, error) { return 0, nil }
func (f *fakeResealer) ResealFields(ctx context.Context) (storagedb.ResealReport, error) {
	return f.run(ctx)
}
func (f *fakeResealer) ResealKeys(ctx context.Context) (storagedb.ResealReport, error) {
	return f.run(ctx)
}

func TestRunSecretReseal_OneReplicaAtATime(t *testing.T) {
	ctx := context.Background()
	fields := &fakeResealer{report: storagedb.ResealReport{Checked: 4, Resealed: 3, Unreadable: 1,
		UnreadableRefs: []storagedb.SealedValueRef{{Table: "org_channels", Column: "url", RowID: "ch-1"}}}}
	keys := &fakeResealer{report: storagedb.ResealReport{Checked: 2, Resealed: 1, Remaining: 1}}

	held := &resealLocker{}
	if _, ran, err := runSecretReseal(ctx, held, fields.ResealFields, keys.ResealKeys); ran || err != nil || fields.calls.Load() != 0 {
		t.Errorf("lock held by another replica: ran %v err %v calls %d, want a skip", ran, err, fields.calls.Load())
	}
	if len(held.keys) != 1 || held.keys[0] != secretResealLockKey {
		t.Errorf("locked keys %v, want the re-seal key", held.keys)
	}
	if _, ran, err := runSecretReseal(ctx, &resealLocker{err: errors.New("conn refused")}, fields.ResealFields); ran || err == nil || fields.calls.Load() != 0 {
		t.Errorf("lock unavailable: ran %v err %v, want a skip with the error", ran, err)
	}

	lock := &resealLocker{acquired: true}
	rep, ran, err := runSecretReseal(ctx, lock, fields.ResealFields, keys.ResealKeys)
	if !ran || err != nil || lock.released.Load() != 1 {
		t.Fatalf("lock acquired: ran %v err %v released %d", ran, err, lock.released.Load())
	}
	if rep.Checked != 6 || rep.Resealed != 4 || rep.Remaining != 1 || rep.Unreadable != 1 || len(rep.UnreadableRefs) != 1 {
		t.Errorf("merged report = %+v", rep)
	}

	failing := &fakeResealer{report: storagedb.ResealReport{Resealed: 2}, err: errors.New("statement timeout")}
	after := &fakeResealer{}
	rep, ran, err = runSecretReseal(ctx, &resealLocker{acquired: true}, failing.ResealFields, after.ResealKeys)
	if !ran || err == nil || rep.Resealed != 2 || after.calls.Load() != 0 {
		t.Errorf("a failing pass: report %+v ran %v err %v later calls %d; want its partial counts, the error, and no later pass",
			rep, ran, err, after.calls.Load())
	}
}

// scheduleSecretReseal runs the pass once when the app starts, only with previous keys configured, and stopping the
// app cancels a run still going, so the shutdown drain does not wait for it.
func TestScheduleSecretReseal_RunsWithPreviousKeysAndStopsWithTheApp(t *testing.T) {
	rotating := config.Default()
	rotating.Auth.EncryptionKey = secret.New("current")
	rotating.Auth.EncryptionKeyPrevious = []secret.Value{secret.New("previous")}

	fields, keys, lock := &fakeResealer{block: true}, &fakeResealer{}, &resealLocker{acquired: true}
	lc := fxtest.NewLifecycle(t)
	scheduleSecretReseal(lc, rotating, fields, keys, lock, nil)
	lc.RequireStart()
	waitUntil(t, "the field pass to start", func() bool { return fields.calls.Load() == 1 })
	lc.RequireStop()
	waitUntil(t, "the cancelled pass to release its lock", func() bool { return lock.released.Load() == 1 })
	if keys.calls.Load() != 0 {
		t.Error("the keystore pass ran after the field pass was cancelled")
	}

	notRotating := config.Default()
	notRotating.Auth.EncryptionKey = secret.New("current")
	idle := &fakeResealer{}
	none := fxtest.NewLifecycle(t)
	scheduleSecretReseal(none, notRotating, idle, idle, lock, nil)
	none.RequireStart()
	none.RequireStop()
	if idle.calls.Load() != 0 {
		t.Error("a pass ran without previous keys")
	}

	desktop := fxtest.NewLifecycle(t)
	scheduleSecretReseal(desktop, rotating, nil, nil, nil, nil) // no database: nothing sealed, no hook
	desktop.RequireStart()
	desktop.RequireStop()
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
