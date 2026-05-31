package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/models"
)

// countingNotifier records how many events it sees, so a test can assert that
// a cancelled-before-begin stream emits nothing.
type countingNotifier struct{ count int64 }

func (n *countingNotifier) Emit(name string, data any) { atomic.AddInt64(&n.count, 1) }

// TestStreamChatMessage_CancelBeforeBegin_ReleasesGoroutine verifies that a
// stream which the client never starts (no /api/chat/begin) is released when
// it is explicitly cancelled, instead of blocking on `<-ctl.started` until the
// 5-minute upper-bound timeout. Pre-fix, the goroutine was stuck waiting on
// the started channel and only cancel-via-timeout could free it.
func TestStreamChatMessage_CancelBeforeBegin_ReleasesGoroutine(t *testing.T) {
	notifier := &countingNotifier{}

	// A factory that always returns an error puts the goroutine into the
	// early-error path, which (after the fix) also goes through awaitStart()
	// and so honors context cancellation.
	factory := ai.NewProviderFactory(
		func(scope, provider string) (string, error) {
			return "", fmt.Errorf("no key configured")
		},
		nil,
	)

	svc := &ChatService{
		ctx:      context.Background(),
		notifier: notifier,
		flow:     &FlowService{ctx: context.Background()},
		analysis: &AnalysisService{ctx: context.Background()},
		factory:  factory,
	}

	id, err := svc.StreamChatMessage("test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	// Without calling BeginStream, the goroutine must be blocked in awaitStart.
	// Cancel it and wait for it to exit (activeStreams entry is deleted on exit).
	svc.CancelStream(id)

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s after CancelStream — likely still blocked on <-ctl.started")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Because the stream was cancelled before being begun, no events should
	// have been emitted. (Pre-fix, an error event was emitted after the
	// goroutine unblocked on the 5-minute timeout.)
	if got := atomic.LoadInt64(&notifier.count); got != 0 {
		t.Errorf("expected 0 events emitted for cancelled-before-begin stream, got %d", got)
	}
}

// TestStreamChatMessage_CancelAfterBegin_EmitsError verifies the normal
// cancellation path: BeginStream is called, then CancelStream interrupts the
// provider call. Since our test factory returns an error before reaching the
// real provider, we expect at least one event (the error emit).
func TestStreamChatMessage_CancelAfterBegin_EmitsError(t *testing.T) {
	notifier := &countingNotifier{}

	factory := ai.NewProviderFactory(
		func(scope, provider string) (string, error) {
			return "", fmt.Errorf("no key configured")
		},
		nil,
	)

	svc := &ChatService{
		ctx:      context.Background(),
		notifier: notifier,
		flow:     &FlowService{ctx: context.Background()},
		analysis: &AnalysisService{ctx: context.Background()},
		factory:  factory,
	}

	id, err := svc.StreamChatMessage("test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	svc.BeginStream(id)

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s after BeginStream")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := atomic.LoadInt64(&notifier.count); got == 0 {
		t.Errorf("expected at least one error event after BeginStream, got 0")
	}
}

// TestBeginStream_ConcurrentCalls_NoPanic verifies that the sync.Once
// guarding `close(ctl.started)` prevents a double-close panic when two
// /api/chat/begin requests for the same streamID race. Pre-fix, the
// `select { default: close }` pattern was single-caller-safe but two
// concurrent callers could both fall through to close → panic.
func TestBeginStream_ConcurrentCalls_NoPanic(t *testing.T) {
	notifier := &countingNotifier{}
	factory := ai.NewProviderFactory(
		func(scope, provider string) (string, error) { return "", fmt.Errorf("no key") },
		nil,
	)
	svc := &ChatService{
		ctx:      context.Background(),
		notifier: notifier,
		flow:     &FlowService{ctx: context.Background()},
		analysis: &AnalysisService{ctx: context.Background()},
		factory:  factory,
	}

	id, err := svc.StreamChatMessage("test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	// Fire many concurrent BeginStream calls for the same ID. With the
	// previous select/default pattern this would intermittently panic with
	// "close of closed channel"; with sync.Once it's safe.
	const N = 50
	done := make(chan struct{}, N)
	for range N {
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("BeginStream panicked: %v", rec)
				}
				done <- struct{}{}
			}()
			svc.BeginStream(id)
		}()
	}
	for range N {
		<-done
	}

	// Cleanup: cancel the stream and wait for goroutine exit.
	svc.CancelStream(id)
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
