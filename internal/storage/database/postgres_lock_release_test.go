package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// lockConnsAfterTwoAcquisitions takes and releases the same advisory key twice on a one-connection fake pool
// and returns the pooled connection each acquisition ran on. failFirstUnlock makes the first release's
// pg_advisory_unlock fail, as a timed-out or broken session would.
func lockConnsAfterTwoAcquisitions(t *testing.T, failFirstUnlock bool) []int {
	t.Helper()
	var unlockFails atomic.Bool
	unlockFails.Store(failFirstUnlock)
	b, f := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
		switch {
		case strings.Contains(query, "pg_try_advisory_lock"):
			return fakeResult{cols: []string{"pg_try_advisory_lock"}, rows: [][]driver.Value{{true}}}
		case strings.Contains(query, "pg_advisory_unlock") && unlockFails.Load():
			return fakeResult{err: errors.New("unlock timed out")}
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
		unlockFails.Store(false)
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
	if conns := lockConnsAfterTwoAcquisitions(t, true); conns[0] == conns[1] {
		t.Fatalf("the session whose unlock failed was reused (connections %v)", conns)
	}
}

// TestReleaseAdvisoryLock_CleanUnlockReusesConnection pins the normal path: a clean unlock pools the connection.
func TestReleaseAdvisoryLock_CleanUnlockReusesConnection(t *testing.T) {
	if conns := lockConnsAfterTwoAcquisitions(t, false); conns[0] != conns[1] {
		t.Fatalf("a cleanly unlocked session was discarded (connections %v)", conns)
	}
}
