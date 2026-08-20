package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/metrics"
	storageif "pad-analyzer/internal/storage/interfaces"
)

func newTestSpiller(t *testing.T, maxBytes int64) *auditSpillStore {
	t.Helper()
	dir := t.TempDir()
	s, err := newAuditSpiller(dir, maxBytes)
	if err != nil {
		t.Fatalf("newAuditSpiller: %v", err)
	}
	return s
}

func ev(id string) *storageif.AuditEvent {
	return &storageif.AuditEvent{ID: id, Action: "test"}
}

// Spill then Drain returns events oldest-first (FIFO).
func TestAuditSpiller_FIFO(t *testing.T) {
	s := newTestSpiller(t, 0)
	for _, e := range []*storageif.AuditEvent{ev("a"), ev("b"), ev("c")} {
		if !s.Spill(e) {
			t.Fatalf("Spill(%s) returned false", e.ID)
		}
	}
	got := s.Drain(2)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("Drain(2) = %v, want [a b]", ids(got))
	}
	got = s.Drain(10)
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("Drain(10) = %v, want [c]", ids(got))
	}
	if s.Drain(10) != nil {
		t.Error("expected nil Drain after emptying")
	}
	if s.HasItems() {
		t.Error("HasItems should be false after full drain")
	}
}

// At the size cap, the newest event is rejected (degrades to log fallback) and
// the drop metric is bumped — sustained overload beyond the spill cap is
// observable, not silent.
func TestAuditSpiller_SizeCapDropsNewest(t *testing.T) {
	// Size the cap from one event's serialised line so exactly one fits.
	sample, _ := json.Marshal(ev("x"))
	oneLine := int64(len(sample) + 1) // + newline
	s := newTestSpiller(t, oneLine+1) // one fits (with 1 byte slack), two don't

	before := metrics.AuditSpillDroppedCount()
	if !s.Spill(ev("x")) {
		t.Fatal("first Spill should fit under the cap")
	}
	if s.Spill(ev("y")) {
		t.Error("second Spill over the cap should be rejected")
	}
	if n := metrics.AuditSpillDroppedCount() - before; n != 1 {
		t.Errorf("spill_dropped_total incremented by %v, want 1", n)
	}
}

// reSpill re-appends drained events WITHOUT bumping the spilled metric (these
// were already counted when first spilled — a replay retrial isn't a new
// overflow).
func TestAuditSpiller_reSpillNoDoubleCount(t *testing.T) {
	s := newTestSpiller(t, 0)
	s.Spill(ev("a"))
	s.Spill(ev("b"))
	drained := s.Drain(2)
	if len(drained) != 2 {
		t.Fatalf("Drain = %d, want 2", len(drained))
	}

	spilledBefore := metrics.AuditSpilledCount()
	s.reSpill(drained)
	if n := metrics.AuditSpilledCount() - spilledBefore; n != 0 {
		t.Errorf("reSpill bumped pad_audit_spilled_total by %v, want 0 (no double-count)", n)
	}
	// The re-spilled events come back out on the next drain, oldest-first.
	got := s.Drain(2)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("after reSpill, Drain = %v, want [a b]", ids(got))
	}
}

// replaySpilled moves events from the spill store into the in-memory pool while
// the pool has headroom. This is the core overflow→replay contract: an event
// that spilled during a burst reaches the channel once capacity returns.
func TestReplaySpilled_DrainsIntoPool(t *testing.T) {
	s := newTestSpiller(t, 0)
	for _, e := range []*storageif.AuditEvent{ev("a"), ev("b"), ev("c")} {
		s.Spill(e)
	}

	// Swap in a fresh pool so the test doesn't depend on process-global state.
	// auditQueueSize/2 is the headroom gate; a cap-4 channel keeps len well
	// under 128 so replay proceeds.
	prevCh := auditCh
	prevSpiller := auditSpiller
	auditCh = make(chan *storageif.AuditEvent, 4)
	auditSpiller = s
	t.Cleanup(func() {
		auditCh = prevCh
		auditSpiller = prevSpiller
	})

	replaySpilled()

	// All three events should have moved into the channel.
	close(auditCh) // safe: this is our test channel, not the real pool
	var got []string
	for e := range auditCh {
		got = append(got, e.ID)
	}
	if len(got) != 3 {
		t.Fatalf("pool received %d events, want 3: %v", len(got), got)
	}
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("pool order = %v, want [a b c] (FIFO preserved)", got)
	}
	if s.HasItems() {
		t.Error("spill store should be empty after a full replay")
	}
}

// replaySpilled yields when the pool is past half-full, leaving headroom for
// fresh events instead of monopolising the channel with a backlog.
func TestReplaySpilled_YieldsWhenPoolHalfFull(t *testing.T) {
	s := newTestSpiller(t, 0)
	s.Spill(ev("a"))
	s.Spill(ev("b"))

	prevCh := auditCh
	prevSpiller := auditSpiller
	// Pre-fill the channel past the auditQueueSize/2 headroom gate. Use the real
	// auditQueueSize capacity and fill >128 so replay's guard fires immediately.
	auditCh = make(chan *storageif.AuditEvent, auditQueueSize)
	for i := 0; i < auditQueueSize/2+1; i++ {
		auditCh <- ev("filler")
	}
	auditSpiller = s
	t.Cleanup(func() {
		auditCh = prevCh
		auditSpiller = prevSpiller
	})

	replaySpilled()

	// Nothing replayed — the spill store still holds its events.
	if !s.HasItems() {
		t.Error("replay should have left events on disk when the pool lacks headroom")
	}
}

func ids(es []*storageif.AuditEvent) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

// TestAuditSpiller_DepthGauge locks in the pad_audit_spill_depth metric wiring:
// spill increments depth, drain decrements it. Pairs with the gauge so on-call
// can see the on-disk backlog without reading the spill file.
func TestAuditSpiller_DepthGauge(t *testing.T) {
	s := newTestSpiller(t, 0)
	for i := 0; i < 3; i++ {
		if !s.Spill(ev("d")) {
			t.Fatalf("spill %d rejected", i)
		}
	}
	if s.depth != 3 {
		t.Errorf("depth after 3 spills = %d, want 3", s.depth)
	}
	if got := metrics.AuditSpillDepthCount(); got != 3 {
		t.Errorf("pad_audit_spill_depth = %v, want 3", got)
	}
	if d := s.Drain(2); len(d) != 2 {
		t.Fatalf("drain = %d, want 2", len(d))
	}
	if s.depth != 1 {
		t.Errorf("depth after draining 2 = %d, want 1", s.depth)
	}
	if got := metrics.AuditSpillDepthCount(); got != 1 {
		t.Errorf("pad_audit_spill_depth after drain = %v, want 1", got)
	}
}

// TestAuditSpiller_DrainRewriteFailureKeepsFileIntact is the regression gate
// for the drain-rewrite swallow: Drain previously ignored the rewrite error,
// so a failed rewrite returned events that were still on disk — the reaper
// replayed them (duplicate audit events) while the depth gauge had already
// excluded them. With the atomic temp+rename rewrite, a failure leaves the
// original file fully intact and Drain reports nothing drained.
func TestAuditSpiller_DrainRewriteFailureKeepsFileIntact(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), auditSpillFileName)
	if err := os.WriteFile(srcPath, []byte("{\"id\":\"a\"}\n{\"id\":\"b\"}\n"), 0o600); err != nil {
		t.Fatalf("seed spill file: %v", err)
	}
	st := &auditSpillStore{path: srcPath, maxBytes: 1 << 20, depth: 2}

	// Deterministic failure injection: pre-create the temp-rewrite path as a
	// directory so WriteFile(tmp) fails with EISDIR. The original file must
	// remain intact, nothing drained, depth unchanged.
	if err := os.Mkdir(srcPath+".drain.tmp", 0o700); err != nil {
		t.Fatalf("block temp write: %v", err)
	}
	if got := st.Drain(10); len(got) != 0 {
		t.Fatalf("expected no events drained on rewrite failure, got %v", ids(got))
	}
	if st.depth != 2 {
		t.Errorf("depth must be unchanged on failed drain, got %d", st.depth)
	}

	// Recovery: unblock, then drain returns both events exactly once — no
	// duplicates, no loss.
	if err := os.Remove(srcPath + ".drain.tmp"); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	got := st.Drain(10)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("recovery drain = %v, want [a b]", ids(got))
	}
	if st.Drain(10) != nil || st.HasItems() {
		t.Error("file must be empty after recovery drain")
	}
}
