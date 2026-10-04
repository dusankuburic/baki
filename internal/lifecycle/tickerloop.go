// Package lifecycle holds small, generic scheduling primitives shared by the
// app's background loops (the governance scanner, the retention purge, …) —
// one place to get the shutdown-race handling right, extracted from two
// identical reimplementations of "cancelable ticker with panic recovery".
package lifecycle

import (
	"context"
	"sync"
	"time"
)

// stopDrainBudget bounds how long Stop waits for an in-flight fn to return.
// Without it Stop returned the instant it signalled, so fx would proceed to
// close the database pool while a governance sweep or an ingest pass was
// still querying it — a guaranteed failure on every shutdown that looks like
// a transient one. Well inside the app's 25s fx.StopTimeout, leaving room
// for the HTTP and WebSocket drains that follow; a tick that outlasts it is
// abandoned (the root context is already cancelled).
const stopDrainBudget = 5 * time.Second

// TickerLoop runs a function on a fixed interval in a background goroutine
// until Stop, with panic recovery so one bad tick can't crash the process.
//
// The context passed to fn derives from a root context that Stop cancels, so
// an in-flight tick can bail out promptly during shutdown — fn should check
// ctx.Err() for this to have effect. Per-tick timeouts (if wanted) are the
// caller's: derive a bounded context from the one TickerLoop passes in.
//
// Start must be called before Stop has any effect; Stop before Start (e.g.
// the caller's own enable-check short-circuited) is safe and a no-op.
type TickerLoop struct {
	// drainBudget overrides stopDrainBudget when non-zero (tests: proving
	// that Stop gives up on a stuck tick need not take the full 5 s). Set
	// before Start; read only by Stop.
	drainBudget time.Duration

	// mu guards the fields below. sync.Once orders other Do calls, NOT a
	// concurrent reader — Stop reading rootCancel/stop while Start writes
	// them inside startOnce.Do was a genuine data race; latent because fx
	// orders OnStart before OnStop, but nothing in this type said so.
	mu         sync.Mutex
	rootCtx    context.Context
	rootCancel context.CancelFunc
	stop       chan struct{}
	// done is closed when run returns, so Stop can wait for an in-flight fn.
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// Start launches fn on the given interval in a background goroutine. If
// runImmediately is true, fn also runs once immediately, before the first
// tick — the "sweep right away on startup" behaviour some loops want (others
// deliberately don't: waiting for the first tick avoids duplicate work right
// after a restart when the interval is short). onPanic, if non-nil, is called
// with the recovered value if fn panics.
//
// A panic that escapes fn ENDS the loop — reported once, then the goroutine
// exits (TestTickerLoop_PanicInFn_IsRecoveredButEndsTheLoop). The backstop is
// for an unexpected panic in the scaffolding; callers that want to survive a
// bad ITEM recover around their own per-item work, which both of them do.
//
// Idempotent: a second Start call (any arguments) is a no-op, matching
// sync.Once semantics — callers gating Start behind their own "am I enabled"
// check don't need their own idempotency guard on top.
func (l *TickerLoop) Start(interval time.Duration, runImmediately bool, fn func(ctx context.Context), onPanic func(recovered any)) {
	l.startOnce.Do(func() {
		l.mu.Lock()
		l.rootCtx, l.rootCancel = context.WithCancel(context.Background())
		l.stop = make(chan struct{})
		l.done = make(chan struct{})
		rootCtx, stop, done := l.rootCtx, l.stop, l.done
		l.mu.Unlock()
		go l.run(rootCtx, stop, done, interval, runImmediately, fn, onPanic)
	})
}

// run owns the loop. It takes the channels as parameters rather than reading
// the struct, so it needs no lock and cannot observe a half-initialised value.
func (l *TickerLoop) run(rootCtx context.Context, stop, done chan struct{}, interval time.Duration, runImmediately bool, fn func(ctx context.Context), onPanic func(recovered any)) {
	defer close(done)

	// Function-scoped ON PURPOSE: a panic that escapes fn is reported once and
	// ENDS the loop. This is a backstop, not a "keep going no matter what"
	// supervisor — both callers already recover around their own per-item work
	// (the ingester per flow, the scanner per sweep), so a panic reaching here
	// means the loop's own scaffolding is broken and continuing to tick would
	// just repeat it (TestTickerLoop_PanicInFn_IsRecoveredButEndsTheLoop).
	defer func() {
		if r := recover(); r != nil && onPanic != nil {
			onPanic(r)
		}
	}()
	if runImmediately {
		fn(rootCtx)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			fn(rootCtx)
		}
	}
}

// Stop cancels the root context (so an in-flight fn call sees it via
// ctx.Err()) and ends the loop. Idempotent, and safe to call even if Start was
// never called — a caller that only conditionally starts the loop can still
// unconditionally call Stop during its own shutdown.
func (l *TickerLoop) Stop() {
	l.stopOnce.Do(func() {
		l.mu.Lock()
		cancel, stop, done := l.rootCancel, l.stop, l.done
		l.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if stop != nil {
			close(stop)
		}
		if done == nil {
			return // never started
		}
		// Wait for the in-flight tick so the caller's dependencies (the DB pool,
		// the blob client) are still alive while it unwinds.
		budget := stopDrainBudget
		if l.drainBudget > 0 {
			budget = l.drainBudget
		}
		select {
		case <-done:
		case <-time.After(budget):
		}
	})
}
