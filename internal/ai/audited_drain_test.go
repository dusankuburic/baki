package ai

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestAudited_UsageWritesRunOnTheDrainedRunner: usage rows used to be written by bare goroutines that shutdown
// never waited for, so a deploy cut off the spend records of the requests it had just finished. They now run on
// usageRecords, which shutdown drains (lifecycle.DrainAll). Draining it here would close it for the rest of the
// package's tests, so the test checks that a blocked write is counted there instead.
func TestAudited_UsageWritesRunOnTheDrainedRunner(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "m"}},
		resp:   &Response{TokensIn: 10, TokensOut: 10},
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	rec := func(context.Context, *interfaces.UsageMetric) error {
		started <- struct{}{}
		<-release
		return nil
	}
	before := usageRecords.Running()
	ap := NewAuditedProvider(stub, rec, "user-1", "stub")
	if _, err := ap.Chat(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("usage write never started")
	}
	if got := usageRecords.Running(); got != before+1 {
		t.Errorf("usageRecords running %d, want %d: the write is not on the runner shutdown drains", got, before+1)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for usageRecords.Running() != before && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := usageRecords.Running(); got != before {
		t.Errorf("usageRecords running %d after the write finished, want %d", got, before)
	}
}
