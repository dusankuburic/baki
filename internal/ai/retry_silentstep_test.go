package ai

// R-provider-7: a retried attempt never inherits a SilentStep its failed predecessor opened.

import (
	"context"
	"testing"
	"time"
)

// silentAttemptStub replays attempts[i] on the i-th Stream call.
type silentAttemptStub struct {
	Provider
	attempts [][]Chunk
	calls    int
}

func (s *silentAttemptStub) ID() string { return "attempt-stub" }
func (s *silentAttemptStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	i := s.calls
	s.calls++
	for _, c := range s.attempts[i] {
		onChunk(c)
	}
	return nil
}

func TestRetryStream_ClosesTheFailedAttemptsSilentSteps(t *testing.T) {
	stub := &silentAttemptStub{attempts: [][]Chunk{
		{{SilentStep: 1}, {Error: ErrProviderDown}},
		{{SilentStep: 1}, {SilentStep: -1}, {Text: "hi"}, {Done: true}},
	}}
	rp := NewRetryingProviderWithConfig(stub, 3, time.Millisecond)
	var steps []int
	open, sawText := 0, false
	err := rp.Stream(context.Background(), Request{}, func(c Chunk) {
		if c.SilentStep != 0 {
			steps = append(steps, int(c.SilentStep))
			open += int(c.SilentStep)
		}
		if c.Text != "" {
			sawText = true
			if open != 0 {
				t.Errorf("at the retried attempt's first text %d silent steps are open, want 0", open)
			}
		}
	})
	if err != nil || stub.calls != 2 || !sawText {
		t.Fatalf("err %v after %d calls (text %v), want success on the second", err, stub.calls, sawText)
	}
	if open != 0 {
		t.Errorf("net SilentStep = %d (steps %v), want 0", open, steps)
	}
	if len(steps) < 2 || steps[0] != 1 || steps[1] != -1 {
		t.Errorf("steps = %v, want the failed attempt's +1 closed before the next attempt's chunks", steps)
	}
}
