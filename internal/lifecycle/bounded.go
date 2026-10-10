package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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
	name     string
	sem      chan struct{}
	inflight atomic.Int64

	mu     sync.Mutex // orders Go's wg.Add against Drain's wg.Wait
	closed bool
	wg     sync.WaitGroup
}

// NewBounded returns a runner allowing at most limit concurrent tasks, registered with the process-wide group
// that shutdown drains (DrainAll). name is the task label on the pad_background_* metrics, in logs and as the
// panic location; it must be a code literal (a closed set), never request data.
func NewBounded(name string, limit int) *Bounded {
	return defaultGroup.NewBounded(name, limit)
}

// Group is a set of runners drained together. Production runners join the process-wide group through
// NewBounded, and jobs through NewSingleton; a test that drains wants a group of its own, so it cannot close another
// test's runners.
type Group struct {
	mu      sync.Mutex
	runners []*Bounded
	jobs    []*Singleton
}

var defaultGroup = &Group{}

// NewBounded returns a runner allowing at most limit concurrent tasks, registered with g.
func (g *Group) NewBounded(name string, limit int) *Bounded {
	if limit < 1 {
		limit = 1
	}
	b := &Bounded{name: name, sem: make(chan struct{}, limit)}
	g.mu.Lock()
	g.runners = append(g.runners, b)
	g.mu.Unlock()
	return b
}

// DrainAll drains every runner created by NewBounded, and stops every job created by NewSingleton that its owner has
// not stopped; shutdown calls it once no request can start new work.
func DrainAll(ctx context.Context) error { return defaultGroup.Drain(ctx) }

// Drain stops g's jobs first: a job's run is cancelled rather than waited out (jobs resume on their next run), then
// waited for while it unwinds, since it may still hand work to a runner. Then it waits until no runner of g has a
// task running, or ctx ends, and only then closes them all: a task may still hand work to another runner (an alert
// fan-out enqueues its email), and closing that runner first would drop the hand-off. Returns the first error, i.e.
// ctx.Err() when runs or tasks were still going.
func (g *Group) Drain(ctx context.Context) error {
	g.mu.Lock()
	runners := append([]*Bounded(nil), g.runners...)
	jobs := append([]*Singleton(nil), g.jobs...)
	g.mu.Unlock()

	var first error
	for _, j := range jobs {
		j.halt()
	}
	for _, j := range jobs {
		if err := j.wait(ctx); err != nil && first == nil {
			first = fmt.Errorf("job %s: %w", j.name, err)
		}
	}

	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
quiesce:
	for !allIdle(runners) {
		select {
		case <-ctx.Done():
			break quiesce
		case <-tick.C:
		}
	}
	for _, b := range runners {
		if err := b.Drain(ctx); err != nil && first == nil {
			first = fmt.Errorf("background %s: %w", b.name, err)
		}
	}
	return first
}

// removeJob takes a stopped job out of g.
func (g *Group) removeJob(s *Singleton) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, j := range g.jobs {
		if j == s {
			g.jobs = append(g.jobs[:i], g.jobs[i+1:]...)
			return
		}
	}
}

func allIdle(runners []*Bounded) bool {
	for _, b := range runners {
		if b.inflight.Load() > 0 {
			return false
		}
	}
	return true
}

// Go runs fn in its own goroutine if a slot is free and reports whether it
// did. It never blocks: when the cap is reached, or once Drain has begun, fn
// is dropped, logged and counted. A panic in fn is recovered and reported like
// every other recovered panic (ERROR log, pad_panics_total, the error sink),
// with the runner's name as its location.
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
	b.inflight.Add(1)
	b.mu.Unlock()
	b.start(fn)
	return true
}

// GoWait is Go for a caller that can afford to wait, a background sweep: at the cap it blocks until a slot is free
// instead of dropping fn, so the sweep slows down. It drops fn (logged and counted, false) when ctx ends first or
// the runner is draining.
func (b *Bounded) GoWait(ctx context.Context, fn func()) bool {
	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		b.drop("cancelled")
		return false
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		<-b.sem
		b.drop("closed")
		return false
	}
	b.wg.Add(1)
	b.inflight.Add(1)
	b.mu.Unlock()
	b.start(fn)
	return true
}

// start runs fn, which holds a slot, in its own goroutine.
func (b *Bounded) start(fn func()) {
	recordBackgroundStarted(b.name)
	start := time.Now()
	go func() {
		defer b.wg.Done()
		defer func() { <-b.sem }()
		defer b.inflight.Add(-1)
		defer func() { recordBackgroundFinished(b.name, time.Since(start)) }()
		defer logger.GuardRecover(b.name)
		fn()
	}()
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

// Running reports how many of b's tasks are in flight.
func (b *Bounded) Running() int {
	return int(b.inflight.Load())
}

func (b *Bounded) drop(reason string) {
	metrics.RecordBackgroundDropped(b.name, reason)
	logger.Warn("background task dropped", "task", b.name, "reason", reason, "limit", cap(b.sem))
}
