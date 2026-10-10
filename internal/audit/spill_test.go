package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
)

func newTestSpill(t *testing.T, maxBytes int64) *spillStore {
	t.Helper()
	s, err := newSpillStore(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatalf("newSpillStore: %v", err)
	}
	return s
}

func ev(id string) *storageif.AuditEvent {
	return &storageif.AuditEvent{ID: id, Action: "test"}
}

// Spill then Drain returns events oldest-first (FIFO).
func TestSpill_FIFO(t *testing.T) {
	s := newTestSpill(t, 0)
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

// The v33 fields survive the spill file, so a replayed row keeps its organisation and actor.
func TestSpill_KeepsActorAndOrg(t *testing.T) {
	s := newTestSpill(t, 0)
	s.Spill(&storageif.AuditEvent{ID: "x", Action: "test", OrgID: "org-1", ActorKind: "system", ActorJob: "scanner", TokenID: "tok", UserAgent: "ua"})
	got := s.Drain(1)
	if len(got) != 1 || got[0].OrgID != "org-1" || got[0].ActorKind != "system" || got[0].ActorJob != "scanner" ||
		got[0].TokenID != "tok" || got[0].UserAgent != "ua" {
		t.Fatalf("Drain = %+v", got)
	}
}

// At the size cap, the newest event is rejected (degrades to log fallback) and the drop metric is bumped —
// sustained overload beyond the spill cap is observable, not silent.
func TestSpill_SizeCapDropsNewest(t *testing.T) {
	// Size the cap from one event's serialised line so exactly one fits.
	sample, _ := json.Marshal(ev("x"))
	oneLine := int64(len(sample) + 1) // + newline
	s := newTestSpill(t, oneLine+1)   // one fits (with 1 byte slack), two don't

	before := auditSpillDroppedCount()
	if !s.Spill(ev("x")) {
		t.Fatal("first Spill should fit under the cap")
	}
	if s.Spill(ev("y")) {
		t.Error("second Spill over the cap should be rejected")
	}
	if n := auditSpillDroppedCount() - before; n != 1 {
		t.Errorf("spill_dropped_total incremented by %v, want 1", n)
	}
}

// reSpill re-appends drained events WITHOUT bumping the spilled metric (these were already counted when first
// spilled — a replay retrial isn't a new overflow).
func TestSpill_reSpillNoDoubleCount(t *testing.T) {
	s := newTestSpill(t, 0)
	s.Spill(ev("a"))
	s.Spill(ev("b"))
	drained := s.Drain(2)
	if len(drained) != 2 {
		t.Fatalf("Drain = %d, want 2", len(drained))
	}

	spilledBefore := auditSpilledCount()
	s.reSpill(drained)
	if n := auditSpilledCount() - spilledBefore; n != 0 {
		t.Errorf("reSpill bumped pad_audit_spilled_total by %v, want 0 (no double-count)", n)
	}
	got := s.Drain(2)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("after reSpill, Drain = %v, want [a b]", ids(got))
	}
}

// replaySpilled moves events from the spill store into the in-memory pool while the pool has headroom. This
// is the core overflow→replay contract: an event that spilled during a burst reaches the queue once capacity
// returns.
func TestReplaySpilled_DrainsIntoPool(t *testing.T) {
	s := newTestSpill(t, 0)
	for _, e := range []*storageif.AuditEvent{ev("a"), ev("b"), ev("c")} {
		s.Spill(e)
	}
	r := &Recorder{queue: make(chan *storageif.AuditEvent, 4), spill: s}

	r.replaySpilled()

	close(r.queue)
	var got []string
	for e := range r.queue {
		got = append(got, e.ID)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("pool received %v, want [a b c] (FIFO preserved)", got)
	}
	if s.HasItems() {
		t.Error("spill store should be empty after a full replay")
	}
}

// replaySpilled yields when the pool is past half-full, leaving headroom for fresh events instead of
// monopolising the queue with a backlog.
func TestReplaySpilled_YieldsWhenPoolHalfFull(t *testing.T) {
	s := newTestSpill(t, 0)
	s.Spill(ev("a"))
	s.Spill(ev("b"))
	r := &Recorder{queue: make(chan *storageif.AuditEvent, queueSize), spill: s}
	for i := 0; i < queueSize/2+1; i++ {
		r.queue <- ev("filler")
	}

	r.replaySpilled()

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

// TestSpill_DepthGauge locks in the pad_audit_spill_depth metric wiring: spill increments depth, drain
// decrements it.
func TestSpill_DepthGauge(t *testing.T) {
	s := newTestSpill(t, 0)
	for i := 0; i < 3; i++ {
		if !s.Spill(ev("d")) {
			t.Fatalf("spill %d rejected", i)
		}
	}
	if s.depth != 3 {
		t.Errorf("depth after 3 spills = %d, want 3", s.depth)
	}
	if got := auditSpillDepthCount(); got != 3 {
		t.Errorf("pad_audit_spill_depth = %v, want 3", got)
	}
	if d := s.Drain(2); len(d) != 2 {
		t.Fatalf("drain = %d, want 2", len(d))
	}
	if s.depth != 1 {
		t.Errorf("depth after draining 2 = %d, want 1", s.depth)
	}
	if got := auditSpillDepthCount(); got != 1 {
		t.Errorf("pad_audit_spill_depth after drain = %v, want 1", got)
	}
}

// TestSpill_DrainRewriteFailureKeepsFileIntact: a failed rewrite must leave the original file fully intact and
// Drain report nothing drained — returning events still on disk would replay them twice.
func TestSpill_DrainRewriteFailureKeepsFileIntact(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), spillFileName)
	if err := os.WriteFile(srcPath, []byte("{\"ID\":\"a\"}\n{\"ID\":\"b\"}\n"), 0o600); err != nil {
		t.Fatalf("seed spill file: %v", err)
	}
	st := &spillStore{path: srcPath, maxBytes: 1 << 20, depth: 2}

	// Deterministic failure injection: pre-create the temp-rewrite path as a directory so WriteFile(tmp) fails.
	if err := os.Mkdir(srcPath+".drain.tmp", 0o700); err != nil {
		t.Fatalf("block temp write: %v", err)
	}
	if got := st.Drain(10); len(got) != 0 {
		t.Fatalf("expected no events drained on rewrite failure, got %v", ids(got))
	}
	if st.depth != 2 {
		t.Errorf("depth must be unchanged on failed drain, got %d", st.depth)
	}

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
