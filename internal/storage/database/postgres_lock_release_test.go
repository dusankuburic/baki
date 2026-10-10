package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/metrics"
)

// firstUnlock is what the first release's pg_advisory_unlock answers.
type firstUnlock int

const (
	unlockHeld    firstUnlock = iota // true: the session held the lock and gave it up
	unlockFails                      // an error, as a timed-out or broken session gives
	unlockNotHeld                    // false: the session did not hold the lock (lost, or a pooler's other session)
)

// lockConnsAfterTwoAcquisitions takes and releases the same advisory key twice on a one-connection fake pool
// and returns the pooled connection each acquisition ran on. first is what the first release's unlock answers;
// the second answers true.
func lockConnsAfterTwoAcquisitions(t *testing.T, first firstUnlock) []int {
	t.Helper()
	var answer atomic.Int32
	answer.Store(int32(first))
	b, f := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
		switch {
		case strings.Contains(query, "pg_try_advisory_lock"):
			return fakeResult{cols: []string{"pg_try_advisory_lock"}, rows: [][]driver.Value{{true}}}
		case strings.Contains(query, "pg_advisory_unlock"):
			switch firstUnlock(answer.Load()) {
			case unlockFails:
				return fakeResult{err: errors.New("unlock timed out")}
			case unlockNotHeld:
				return fakeResult{cols: []string{"pg_advisory_unlock"}, rows: [][]driver.Value{{false}}}
			}
			return fakeResult{cols: []string{"pg_advisory_unlock"}, rows: [][]driver.Value{{true}}}
		}
		return fakeResult{}
	})
	ctx := context.Background()
	for i := range 2 {
		release, ok, err := b.TryGlobalLock(ctx, 42)
		if err != nil || !ok {
			t.Fatalf("acquisition %d: acquired=%v err=%v", i+1, ok, err)
		}
		release()
		answer.Store(int32(unlockHeld))
	}
	var conns []int
	for _, c := range f.Queries() {
		if strings.Contains(c.query, "pg_try_advisory_lock") {
			conns = append(conns, c.conn)
		}
	}
	if len(conns) != 2 {
		t.Fatalf("want 2 lock statements, got %d", len(conns))
	}
	return conns
}

// TestReleaseAdvisoryLock_FailedUnlockDiscardsConnection: a failed unlock leaves the lock on the session, so that
// session must never return to the pool, where it would carry the lock and stall the job on every replica.
func TestReleaseAdvisoryLock_FailedUnlockDiscardsConnection(t *testing.T) {
	if conns := lockConnsAfterTwoAcquisitions(t, unlockFails); conns[0] == conns[1] {
		t.Fatalf("the session whose unlock failed was reused (connections %v)", conns)
	}
}

// TestReleaseAdvisoryLock_CleanUnlockReusesConnection pins the normal path: a clean unlock pools the connection.
func TestReleaseAdvisoryLock_CleanUnlockReusesConnection(t *testing.T) {
	if conns := lockConnsAfterTwoAcquisitions(t, unlockHeld); conns[0] != conns[1] {
		t.Fatalf("a cleanly unlocked session was discarded (connections %v)", conns)
	}
}

// TestReleaseAdvisoryLock_UnlockFalseIsLostLease: pg_advisory_unlock answering false means the session did not hold
// the lock any more: it was lost while held (another replica may have run the job beside this one), or a
// transaction-mode pooler ran the unlock on another server session, where the lock now leaks. It used to pass for a
// clean release. It is counted, and the session is discarded: whatever it holds goes with it (R4).
func TestReleaseAdvisoryLock_UnlockFalseIsLostLease(t *testing.T) {
	before := metrics.AdvisoryUnlockNotHeldCount()
	if conns := lockConnsAfterTwoAcquisitions(t, unlockNotHeld); conns[0] == conns[1] {
		t.Fatalf("the session whose unlock reported the lock not held was reused (connections %v)", conns)
	}
	if got := metrics.AdvisoryUnlockNotHeldCount() - before; got != 1 {
		t.Errorf("pad_advisory_unlock_not_held_total rose by %v, want 1", got)
	}
}
