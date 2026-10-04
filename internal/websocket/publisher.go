package websocket

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-core/logger"
)

const (
	// publishQueueCap bounds the envelopes waiting for the backplane publish;
	// a full queue drops the envelope for peer replicas (counted on
	// pad_background_dropped_total{task="ws_publish"}) rather than blocking
	// the caller.
	publishQueueCap = 1024
	// publishDrainBudget bounds how long close keeps publishing what is still
	// queued (each client's presence.leave; the caller's context cuts it
	// shorter).
	publishDrainBudget = 2 * time.Second
	// publishCloseFallback bounds close's wait for a caller whose context has
	// no deadline (Hub.Close): the drain plus one in-flight publish (the
	// backplane times out after 2 s).
	publishCloseFallback = publishDrainBudget + 2*time.Second
	// publishDropWarnEvery rate-limits the "queue full" log line.
	publishDropWarnEvery = 5 * time.Second
)

// publisher hands room broadcasts to the backplane from one goroutine of its
// own, so callers of Hub.Broadcast never wait on Redis (an inline publish
// stalled every pump when Redis stalled). The queue is bounded and never
// blocks; a single sender keeps broadcast order, which peers rely on (a
// presence.update must not overtake the presence.leave that followed it).
type publisher struct {
	b     backplane
	queue chan publishItem
	stop  chan struct{}
	done  chan struct{}

	stopOnce sync.Once
	// drainUntil is when the drain stops publishing (unix nanoseconds), set
	// by close before it closes stop.
	drainUntil atomic.Int64
	dropped    atomic.Int64
	lastWarn   atomic.Int64 // unix nanoseconds of the last drop warning
}

type publishItem struct {
	flowID  string
	envJSON []byte
}

// newPublisher starts the sender goroutine; stop it with close.
func newPublisher(b backplane) *publisher {
	p := &publisher{
		b:     b,
		queue: make(chan publishItem, publishQueueCap),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go p.run()
	return p
}

// publish queues envJSON for the backplane. It never blocks: when the queue is
// full the envelope is dropped for peer replicas, counted and logged.
func (p *publisher) publish(flowID string, envJSON []byte) {
	select {
	case p.queue <- publishItem{flowID: flowID, envJSON: envJSON}:
	default:
		p.drop(flowID)
	}
}

func (p *publisher) drop(flowID string) {
	n := p.dropped.Add(1)
	metrics.RecordBackgroundDropped("ws_publish", "saturated")
	now := time.Now().UnixNano()
	last := p.lastWarn.Load()
	if now-last >= int64(publishDropWarnEvery) && p.lastWarn.CompareAndSwap(last, now) {
		logger.Warn("websocket backplane publish dropped: queue full",
			"flowID", flowID, "queue_cap", publishQueueCap, "dropped_total", n)
	}
}

// run is the sender: one publish at a time, in queue order. On stop it keeps
// publishing what is already queued, until drainUntil, then exits.
func (p *publisher) run() {
	defer close(p.done)
	for {
		select {
		case <-p.stop:
			p.drain()
			return
		case it := <-p.queue:
			p.send(it)
		}
	}
}

func (p *publisher) drain() {
	for {
		select {
		case it := <-p.queue:
			p.send(it)
		default:
			return
		}
	}
}

// send publishes it unless close has set a drain deadline that has passed.
// Checked on the main loop as well as in drain: once stop is closed the
// select above may still take queued items before it takes stop.
func (p *publisher) send(it publishItem) {
	if d := p.drainUntil.Load(); d != 0 && time.Now().UnixNano() > d {
		return
	}
	p.b.publish(it.flowID, it.envJSON)
}

// close stops the sender and waits for it to finish: the in-flight publish
// (bounded by the backplane's timeout) and the drain, until publishDrainBudget
// or ctx's deadline. The wait itself ends with ctx or after
// publishCloseFallback; a sender still busy then is abandoned (Hub.close
// cancels the backplane right after, failing its publish at once). Idempotent.
func (p *publisher) close(ctx context.Context) {
	p.stopOnce.Do(func() {
		deadline := time.Now().Add(publishDrainBudget)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		p.drainUntil.Store(deadline.UnixNano())
		close(p.stop)
	})
	select {
	case <-p.done:
	case <-ctx.Done():
	case <-time.After(publishCloseFallback):
	}
}
