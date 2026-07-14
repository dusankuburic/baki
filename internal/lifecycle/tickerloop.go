// Package lifecycle holds small, generic scheduling primitives shared by the
// app's background loops (the governance scanner, the Power Platform
// connector's ingest sweep, …). Extracted because those two loops had
// independently reimplemented the identical "cancelable ticker with panic
// recovery" scaffolding, down to the same comments explaining why Stop derives
// from a root context — one place to get the shutdown-race handling right.
package lifecycle

import (
	"context"
	"sync"
	"time"
)

// TickerLoop runs a function on a fixed interval in a background goroutine
// until Stop, with panic recovery so one bad tick can't crash the process.
//
// The context passed to fn is derived from a root context that Stop cancels,
// so an in-flight tick can bail out promptly instead of running to completion
// during shutdown — fn should check ctx.Err()/respect cancellation for this to
// have effect. Per-tick timeouts (if the caller wants one) are the caller's
// responsibility: derive a bounded context from the one TickerLoop passes in.
//
// The zero value is not ready to use as-is only in that Start must be called
// before Stop has any effect; calling Stop before Start (or on a TickerLoop
// that never started, e.g. because the caller's own enable-check short-
// circuited) is safe and a no-op.
type TickerLoop struct {
	rootCtx    context.Context
	rootCancel context.CancelFunc
	stop       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
}

// Start launches fn on the given interval in a background goroutine. If
// runImmediately is true, fn also runs once immediately, before the first
// tick — the "sweep right away on startup" behaviour some loops want (and
// others deliberately don't: waiting for the first tick avoids doing
// duplicate work right after a restart for a caller whose interval is short
// relative to typical process lifetime). onPanic, if non-nil, is called with
// the recovered value if fn panics on any invocation; the loop then continues
// to the next tick rather than dying.
//
// Idempotent: a second Start call (any arguments) is a no-op, matching
// sync.Once semantics — callers that gate Start behind their own "am I
// enabled" check don't need their own idempotency guard on top.
func (l *TickerLoop) Start(interval time.Duration, runImmediately bool, fn func(ctx context.Context), onPanic func(recovered any)) {
	l.startOnce.Do(func() {
		l.rootCtx, l.rootCancel = context.WithCancel(context.Background())
		l.stop = make(chan struct{})
		go l.run(interval, runImmediately, fn, onPanic)
	})
}

func (l *TickerLoop) run(interval time.Duration, runImmediately bool, fn func(ctx context.Context), onPanic func(recovered any)) {
	defer func() {
		if r := recover(); r != nil && onPanic != nil {
			onPanic(r)
		}
	}()
	if runImmediately {
		fn(l.rootCtx)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			fn(l.rootCtx)
		}
	}
}

// Stop cancels the root context (so an in-flight fn call sees it via
// ctx.Err()) and ends the loop. Idempotent, and safe to call even if Start was
// never called — a caller that only conditionally starts the loop can still
// unconditionally call Stop during its own shutdown.
func (l *TickerLoop) Stop() {
	l.stopOnce.Do(func() {
		if l.rootCancel != nil {
			l.rootCancel()
		}
		if l.stop != nil {
			close(l.stop)
		}
	})
}
