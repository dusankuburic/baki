package lifecycle

import (
	"context"
	"sync"

	"pad-analyzer/internal/metrics"
	"pad-core/logger"
)

// Bounded runs fire-and-forget background work — an analysis webhook, a
// finding email — with a hard cap on how much of it is in flight, and lets
// shutdown wait for what is.
//
// Each such task is already time-bounded by its own context, but a bare `go`
// per request put no limit on how MANY run at once: while a webhook or SMTP
// endpoint was slow, live goroutines grew with request rate × delivery time
// (a 10 s delivery at 50 req/s is 500 goroutines, each holding its event and
// possibly a DB connection), and none were drained on shutdown. This is the
// scanner's deliverySem + deliveryWG pattern, made reusable. Unlike the
// scanner — a background sweep that can afford to slow down — Go never blocks:
// it is called on request paths, so saturation drops the task instead.
type Bounded struct {
	name string
	sem  chan struct{}

	mu     sync.Mutex // orders Go's wg.Add against Drain's wg.Wait
	closed bool
	wg     sync.WaitGroup
}

// NewBounded returns a runner allowing at most limit concurrent tasks. name
// is the task label on pad_background_dropped_total and in logs; it must be a
// code literal (a closed set), never request data.
func NewBounded(name string, limit int) *Bounded {
	if limit < 1 {
		limit = 1
	}
	return &Bounded{name: name, sem: make(chan struct{}, limit)}
}

// Go runs fn in its own goroutine if a slot is free and reports whether it
// did. It never blocks: when the cap is reached, or once Drain has begun, fn
// is dropped, logged and counted. A panic in fn is recovered and logged.
func (b *Bounded) Go(fn func()) bool {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.drop("closed")
		return false
	}
	select {
	case b.sem <- struct{}{}:
	default:
		b.mu.Unlock()
		b.drop("saturated")
		return false
	}
	b.wg.Add(1)
	b.mu.Unlock()

	go func() {
		defer b.wg.Done()
		defer func() { <-b.sem }()
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("background task panicked", "task", b.name, "panic", r)
			}
		}()
		fn()
	}()
	return true
}

// Drain stops Go from starting new tasks and waits until every started task
// has returned, or ctx ends (returning ctx.Err()). Tasks still running then
// are abandoned — each is bounded by its own timeout — and so is the one
// helper goroutine waiting on them. Idempotent.
func (b *Bounded) Drain(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()

	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Bounded) drop(reason string) {
	metrics.RecordBackgroundDropped(b.name, reason)
	logger.Warn("background task dropped", "task", b.name, "reason", reason, "limit", cap(b.sem))
}
