package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/testutil"
)

// TestJournal_RecordAndBound: only agentic types are journaled, and the
// journal drops the OLDEST entries at capacity (recent events matter most).
func TestJournal_RecordAndBound(t *testing.T) {
	ctl := &streamCtl{}
	ctl.mu.Lock()
	for i := 0; i < maxJournalEvents+20; i++ {
		ctl.recordJournal("tool_result", map[string]interface{}{"name": "t", "i": i})
		ctl.recordJournal("chunk", map[string]interface{}{"content": "not journaled"})
	}
	ctl.mu.Unlock()
	snap := ctl.snapshot()

	if len(snap.Events) > maxJournalEvents {
		t.Fatalf("journal exceeded cap: %d", len(snap.Events))
	}
	// The oldest 60 entries dropped; the newest (index maxJournalEvents+19)
	// must be the last recorded.
	last := snap.Events[len(snap.Events)-1]
	if got, _ := last.Data["i"].(int); got != maxJournalEvents+19 {
		t.Errorf("newest event lost: got i=%v, want %d", got, maxJournalEvents+19)
	}
	for _, ev := range snap.Events {
		if ev.Type == "chunk" {
			t.Fatalf("chunk must never be journaled: %+v", ev)
		}
	}
}

// TestResumeStream_ReplaysJournal: a registered stream's agentic journal rides
// the ResumeResult — the reconnecting client rebuilds its tool trail and any
// pending approval card from these events.
func TestResumeStream_ReplaysJournal(t *testing.T) {
	svc := &ChatService{}
	ctl := &streamCtl{ownerID: "user-1", streamID: "s1"}
	ctl.mu.Lock()
	ctl.recordJournal("tool", map[string]interface{}{"name": "search_flow", "label": "Searching flow"})
	ctl.recordJournal("tool_result", map[string]interface{}{"name": "search_flow", "ok": true, "summary": "1 match"})
	ctl.recordJournal("fix_proposal", map[string]interface{}{"proposalId": "p1", "ruleId": "unhandled-error", "batch": true})
	ctl.mu.Unlock()
	svc.activeStreams.Store("s1", ctl)

	res, err := svc.ResumeStream(context.Background(), "s1", 0)
	if err != nil {
		t.Fatalf("ResumeStream: %v", err)
	}
	if len(res.Events) != 3 {
		t.Fatalf("want 3 replayed events, got %d", len(res.Events))
	}
	if res.Events[0].Type != "tool" || res.Events[2].Type != "fix_proposal" {
		t.Errorf("event order wrong: %+v", res.Events)
	}
	if res.Events[2].Data["batch"] != true {
		t.Errorf("batch proposal payload not preserved: %+v", res.Events[2].Data)
	}

	// The returned slice must not alias the live journal (later events can't
	// mutate an already-delivered resume result).
	ctl.mu.Lock()
	ctl.recordJournal("tool", map[string]interface{}{"name": "late"})
	ctl.mu.Unlock()
	if len(res.Events) != 3 {
		t.Errorf("resume result aliased the live journal: %d events after late append", len(res.Events))
	}
}

// TestBeginStream_ReplaysJournalOverSSE: beginning a live stream that already
// recorded events re-emits them to the owner — a fresh subscriber misses
// everything emitted before it subscribed, and without the replay a pending
// fix proposal would be unreachable (guaranteed 60s timeout after reconnect).
func TestBeginStream_ReplaysJournalOverSSE(t *testing.T) {
	notifier := &testutil.CountingNotifier{}
	svc := &ChatService{notifier: notifier}
	ctl := &streamCtl{ownerID: "user-1", streamID: "s1", started: make(chan struct{})}
	ctl.mu.Lock()
	ctl.recordJournal("fix_proposal", map[string]interface{}{"proposalId": "p1", "ruleId": "r"})
	ctl.recordJournal("tool_result", map[string]interface{}{"name": "search_flow", "ok": true})
	ctl.mu.Unlock()
	svc.activeStreams.Store("s1", ctl)

	notifier.Reset()
	if res := svc.BeginStream(context.Background(), "s1"); res != nil {
		t.Fatalf("live stream must return nil (events over SSE), got %+v", res)
	}
	evs := notifier.Events()
	if len(evs) != 2 {
		t.Fatalf("want 2 replayed chat:event emissions, got %d", len(evs))
	}
	for _, ev := range evs {
		if ev.Name != "chat:event" {
			t.Errorf("unexpected event name %q", ev.Name)
		}
		env, ok := ev.Data.(map[string]interface{})
		if !ok || env["streamId"] != "s1" {
			t.Errorf("replay not stream-addressed: %+v", ev.Data)
		}
	}
	// An empty journal replays nothing (the common begin path stays a no-op).
	ctl2 := &streamCtl{ownerID: "user-1", streamID: "s2", started: make(chan struct{})}
	svc.activeStreams.Store("s2", ctl2)
	notifier.Reset()
	svc.BeginStream(context.Background(), "s2")
	if notifier.Count() != 0 {
		t.Errorf("empty journal replayed %d events", notifier.Count())
	}
}
