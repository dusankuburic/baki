package lifecycle

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTickerLoop_RunImmediateTrue_FiresBeforeFirstTick(t *testing.T) {
	var l TickerLoop
	done := make(chan struct{})
	var once sync.Once
	l.Start(time.Hour, true, func(context.Context) {
		once.Do(func() { close(done) })
	}, nil)
	defer l.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("immediate call did not fire within 2s")
	}
}

func TestTickerLoop_RunImmediateFalse_WaitsForFirstTick(t *testing.T) {
	var l TickerLoop
	var calls atomic.Int32
	l.Start(20*time.Millisecond, false, func(context.Context) {
		calls.Add(1)
	}, nil)
	defer l.Stop()

	// Immediately after Start, nothing should have run yet.
	if n := calls.Load(); n != 0 {
		t.Errorf("calls immediately after Start = %d, want 0 (runImmediately=false)", n)
	}

	time.Sleep(60 * time.Millisecond)
	if n := calls.Load(); n < 1 {
		t.Errorf("calls after waiting for ticks = %d, want >= 1", n)
	}
}

func TestTickerLoop_Stop_CancelsPassedContext(t *testing.T) {
	var l TickerLoop
	canceled := make(chan struct{})
	started := make(chan struct{})
	l.Start(time.Hour, true, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(canceled)
	}, nil)

	<-started
	l.Stop()

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel the context passed to fn within 2s")
	}
}

func TestTickerLoop_Stop_IsIdempotentAndSafeWithoutStart(t *testing.T) {
	var l TickerLoop
	l.Stop() // never started — must not panic or hang
	l.Stop() // second call — must not panic

	var l2 TickerLoop
	l2.Start(time.Hour, false, func(context.Context) {}, nil)
	l2.Stop()
	l2.Stop() // idempotent after Start too
}

// TestTickerLoop_PanicInFn_IsRecoveredButEndsTheLoop pins the exact semantics
// TickerLoop must preserve from the two loops it replaced (Scanner.loop and
// Ingester.loop): a single top-level recover wraps the whole run() body, so a
// panic that escapes fn (i.e. isn't already contained by fn's own inner
// recover — both original callers additionally recover around their actual
// per-item work) is reported via onPanic once and then ENDS the goroutine —
// it does not resume ticking afterward. This is a backstop for an unexpected
// panic, not a "keep going no matter what" supervisor.
func TestTickerLoop_PanicInFn_IsRecoveredButEndsTheLoop(t *testing.T) {
	var l TickerLoop
	var calls atomic.Int32
	panicked := make(chan any, 1)
	l.Start(10*time.Millisecond, true, func(context.Context) {
		calls.Add(1)
		panic("boom")
	}, func(recovered any) {
		panicked <- recovered
	})
	defer l.Stop()

	select {
	case r := <-panicked:
		if r != "boom" {
			t.Errorf("onPanic recovered value = %v, want %q", r, "boom")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onPanic was not called within 2s")
	}

	// The goroutine exited on the immediate call's panic, before the ticker
	// loop even started — give it a window where further ticks WOULD have
	// fired (10ms interval) and confirm calls stayed at exactly 1.
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("calls after panic = %d, want exactly 1 (loop must not resume ticking after a panic)", n)
	}
}
