package service

import (
	"sync"
	"testing"
)

// TestResolveFixDecision_ConcurrentCallersDoNotRace is the regression gate for a
// data race on a shared slice header.
//
// fixDecisionState documented "first decision wins; later sends find the buffer
// full and are dropped", and the per-item opt-out list was written to the state
// struct BEFORE that send on the theory that the channel supplied the
// happens-before edge. It does — but only for the caller whose send actually
// succeeds. EVERY caller performed the write, including the ones dropped into
// the `default:` arm, so two concurrent resolutions raced each other and raced
// the consumer's read, on an unsynchronised slice header. A torn read (new
// pointer, stale length) walks off the backing array; the `i < len(props)`
// bounds check downstream guards the INDICES, not the iteration.
//
// Reachable without any special privilege: a double-clicked Approve on a batch
// card, two tabs open on the same stream, or an ordinary client retry.
//
// Run under -race; without it this test cannot fail.
func TestResolveFixDecision_ConcurrentCallersDoNotRace(t *testing.T) {
	svc, _ := newFixTestService(fixTestDoc(), nil)

	const rounds = 50
	for r := 0; r < rounds; r++ {
		st := &fixDecisionState{streamID: "stream-1", decided: make(chan fixDecision, 1)}
		svc.pendingFixDecisions.Store("prop-race", st)

		var wg sync.WaitGroup
		// Several concurrent approvals, each with its own opt-out list, plus a
		// concurrent consumer — the exact shape of a double-clicked card.
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_ = svc.ResolveFixDecision("stream-1", "prop-race", true, []int{i, i + 1, i + 2})
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			dec := <-st.decided
			// Read the payload the way the batch consumer does.
			total := 0
			for _, idx := range dec.excludedBatchIdx {
				total += idx
			}
			_ = total
		}()
		wg.Wait()
		svc.pendingFixDecisions.Delete("prop-race")
	}
}

// TestResolveFixDecision_WinnersOptOutsAreTheOnesDelivered pins the behaviour the
// race fix must preserve: the opt-out list the consumer sees belongs to the
// decision that won, not to whichever caller happened to write last.
func TestResolveFixDecision_WinnersOptOutsAreTheOnesDelivered(t *testing.T) {
	svc, _ := newFixTestService(fixTestDoc(), nil)
	st := &fixDecisionState{streamID: "stream-1", decided: make(chan fixDecision, 1)}
	svc.pendingFixDecisions.Store("prop-1", st)

	if err := svc.ResolveFixDecision("stream-1", "prop-1", true, []int{1, 2}); err != nil {
		t.Fatalf("first decision: %v", err)
	}
	// A later caller must not be able to change what the winner decided.
	if err := svc.ResolveFixDecision("stream-1", "prop-1", true, []int{7, 8, 9}); err != nil {
		t.Fatalf("second decision should not error: %v", err)
	}

	dec := <-st.decided
	if !dec.approved {
		t.Error("winning decision lost its approval")
	}
	if len(dec.excludedBatchIdx) != 2 || dec.excludedBatchIdx[0] != 1 || dec.excludedBatchIdx[1] != 2 {
		t.Errorf("consumer received %v — a dropped caller's opt-outs overwrote the winner's",
			dec.excludedBatchIdx)
	}
}

// TestResolveFixDecision_DeclineCarriesNoOptOuts keeps the resolver's existing
// rule that opt-outs are meaningless on a decline.
func TestResolveFixDecision_DeclineCarriesNoOptOuts(t *testing.T) {
	svc, _ := newFixTestService(fixTestDoc(), nil)
	st := &fixDecisionState{streamID: "stream-1", decided: make(chan fixDecision, 1)}
	svc.pendingFixDecisions.Store("prop-1", st)

	if err := svc.ResolveFixDecision("stream-1", "prop-1", false, []int{1, 2}); err != nil {
		t.Fatalf("decline: %v", err)
	}
	dec := <-st.decided
	if dec.approved {
		t.Error("decline was delivered as an approval")
	}
	if len(dec.excludedBatchIdx) != 0 {
		t.Errorf("a decline carried opt-outs: %v", dec.excludedBatchIdx)
	}
}
