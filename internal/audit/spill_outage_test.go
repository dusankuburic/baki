package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// sqlStateErr stands for a driver error carrying an SQLSTATE (pgconn.PgError has the same method).
type sqlStateErr string

func (e sqlStateErr) Error() string    { return "pg error " + string(e) }
func (e sqlStateErr) SQLState() string { return string(e) }

// refused is what a write to a database that is down returns: a dial error under the driver's wrapping.
func refused() error {
	return fmt.Errorf("save audit events: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
}

func TestDatabaseUnreachable_Classification(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{refused(), true},
		{context.DeadlineExceeded, true},
		{fmt.Errorf("x: %w", sqlStateErr("08006")), true}, // connection failure
		{sqlStateErr("57P01"), true},                      // admin shutdown
		{sqlStateErr("57P03"), true},                      // cannot connect now
		{sqlStateErr("53300"), true},                      // too many connections
		{sqlStateErr("28P01"), true},                      // the credentials no longer work
		{sqlStateErr("23514"), false},                     // a check violation: the row, not the database
		{sqlStateErr("22P02"), false},
		{errors.New("some other failure"), false},
		{context.Canceled, false},
		{nil, false},
	} {
		if got := databaseUnreachable(c.err); got != c.want {
			t.Errorf("databaseUnreachable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// A batch the database is not there for is spilled whole, without the per-event round that would only wait out the
// same failure once per event; nothing reaches the log fallback.
func TestWriteBatch_UnreachableSpillsTheBatch(t *testing.T) {
	w := &fakeWriter{}
	s := &fakeBatchSink{err: refused()}
	var spilled []string
	before := auditDroppedCount("write_failed")
	writeBatch(w, s, events(3), func(e *storageif.AuditEvent) bool { spilled = append(spilled, e.ID); return true })
	if len(spilled) != 3 {
		t.Errorf("spilled %v, want the 3 events", spilled)
	}
	if w.count() != 0 {
		t.Errorf("per-event writes = %d, want none while the database is unreachable", w.count())
	}
	if n := auditDroppedCount("write_failed") - before; n != 0 {
		t.Errorf("write_failed rose by %v, want 0", n)
	}
}

// A bad row still goes to the log fallback (replaying it would fail forever); a per-event write that finds the
// database gone spills.
func TestWriteBatch_BadRowFallsBackAndUnreachableRowSpills(t *testing.T) {
	s := &fakeBatchSink{err: sqlStateErr("23514")}
	var spilled []string
	spill := func(e *storageif.AuditEvent) bool { spilled = append(spilled, e.ID); return true }

	before := auditDroppedCount("write_failed")
	writeBatch(&fakeWriter{err: sqlStateErr("23514")}, s, events(1), spill)
	if n := auditDroppedCount("write_failed") - before; n != 1 || len(spilled) != 0 {
		t.Errorf("a refused row: write_failed +%v, spilled %v; want +1 and nothing spilled", n, spilled)
	}

	before = auditDroppedCount("write_failed")
	writeBatch(&fakeWriter{err: refused()}, s, events(2), spill)
	if n := auditDroppedCount("write_failed") - before; n != 0 || len(spilled) != 2 {
		t.Errorf("rows the database was gone for: write_failed +%v, spilled %v; want +0 and both spilled", n, spilled)
	}

	// Spill full (or none): the log fallback, as before.
	before = auditDroppedCount("write_failed")
	writeBatch(&fakeWriter{err: refused()}, &fakeBatchSink{err: refused()}, events(1), func(*storageif.AuditEvent) bool { return false })
	if n := auditDroppedCount("write_failed") - before; n != 1 {
		t.Errorf("an unspillable event: write_failed +%v, want +1", n)
	}
}

// outageStore is the fake backend behind a database that can go away.
type outageStore struct {
	*testutil.FakeBackend
	down atomic.Bool
}

func (s *outageStore) SaveAuditEvent(ctx context.Context, e *storageif.AuditEvent) error {
	if s.down.Load() {
		return refused()
	}
	return s.FakeBackend.SaveAuditEvent(ctx, e)
}

func (s *outageStore) SaveAuditEvents(ctx context.Context, es []*storageif.AuditEvent) error {
	if s.down.Load() {
		return refused()
	}
	return s.FakeBackend.SaveAuditEvents(ctx, es)
}

// End to end: during a database outage the recorder's events go to the spill file, not the log; once the database is
// back the reaper replays them, each stored once.
func TestRecorder_DatabaseOutageSpillsAndReplays(t *testing.T) {
	prev := replayPauseAfterOutage
	replayPauseAfterOutage = 300 * time.Millisecond
	t.Cleanup(func() { replayPauseAfterOutage = prev })

	store := &outageStore{FakeBackend: testutil.NewFakeBackend()}
	store.down.Store(true)
	r := New(store, Config{SpillDir: t.TempDir(), Instance: "outage"})
	r.Start()
	t.Cleanup(r.Stop)

	droppedBefore := auditDroppedCount("write_failed")
	spilledBefore := auditSpilledReasonCount(spillReasonUnreachable)
	const n = 5
	for i := range n {
		r.Record(context.Background(), Entry{Action: fmt.Sprintf("during-outage-%d", i)})
	}
	waitFor(t, "the events on the spill file", func() bool {
		r.spill.mu.Lock()
		defer r.spill.mu.Unlock()
		return r.spill.depth == n
	})
	if got := auditDroppedCount("write_failed") - droppedBefore; got != 0 {
		t.Errorf("write_failed rose by %v during the outage, want 0 (spilled instead)", got)
	}
	if got := auditSpilledReasonCount(spillReasonUnreachable) - spilledBefore; got < n {
		t.Errorf("pad_audit_spilled_total{reason=db_unavailable} rose by %v, want at least %d", got, n)
	}

	store.down.Store(false)
	waitFor(t, "the replay after the outage", func() bool { return len(store.SavedAuditEvents()) == n })
	r.Stop()
	seen := map[string]int{}
	for _, e := range store.SavedAuditEvents() {
		seen[e.Action]++
	}
	for i := range n {
		if a := fmt.Sprintf("during-outage-%d", i); seen[a] != 1 {
			t.Errorf("%s stored %d times, want once (%v)", a, seen[a], seen)
		}
	}
}

// A long outage cannot fill the disk: the spill file stops at its cap, and what does not fit is counted and goes to
// the log fallback.
func TestRecorder_OutageSpillIsBounded(t *testing.T) {
	sample, _ := json.Marshal(NewEvent(context.Background(), Entry{Action: "bounded-0"}))
	maxBytes := int64(2*(len(sample)+1) + len(sample)/2) // two events fit (the slack absorbs a timestamp's length), three do not
	store := &outageStore{FakeBackend: testutil.NewFakeBackend()}
	store.down.Store(true)
	r := New(store, Config{SpillDir: t.TempDir(), Instance: "bounded", SpillMaxBytes: maxBytes})
	r.Start()
	t.Cleanup(r.Stop)

	capBefore, failedBefore := auditSpillDroppedCount(), auditDroppedCount("write_failed")
	for i := range 4 {
		r.Record(context.Background(), Entry{Action: fmt.Sprintf("bounded-%d", i)})
	}
	waitFor(t, "every event placed", func() bool { return auditDroppedCount("write_failed")-failedBefore == 2 })
	if got := auditSpillDroppedCount() - capBefore; got != 2 {
		t.Errorf("pad_audit_spill_dropped_total rose by %v, want 2", got)
	}
	r.spill.mu.Lock()
	depth := r.spill.depth
	r.spill.mu.Unlock()
	if depth != 2 {
		t.Errorf("spill depth = %d, want the 2 that fit", depth)
	}
}
