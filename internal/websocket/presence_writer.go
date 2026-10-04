package websocket

import (
	"sync"
	"time"
)

// presenceWriter drains presence-record upserts to the backplane on its own
// goroutine, so no caller waits on a store round trip (a slow Redis used to
// stall read pumps and teardown alike). Records are ephemeral (TTL-limited,
// rewritten by the next heartbeat), so pending upserts are coalesced per
// connection.
//
// Deletes (Leave) are ordered after the upserts enqueued before them: a
// delete that ran beside an in-flight heartbeat batch carrying the same
// record could be undone by that batch landing afterwards — a ghost presence
// until the record's TTL.
type presenceWriter struct {
	b backplane

	mu sync.Mutex
	// pending holds the latest record per connection per flow; a newer
	// enqueue for the same connection replaces the older one, and
	// cancelPending removes one (Leave, before its delete op).
	pending map[string]map[string][]byte
	// deletes are processed in FIFO order after the pending upserts of each
	// drain cycle; each carries a done channel closed once processed (or
	// dropped at close).
	deletes  []deleteOp
	wake     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	stopped  bool
}

type deleteOp struct {
	flowID string
	connID string
	done   chan struct{}
}

func newPresenceWriter(b backplane) *presenceWriter {
	return &presenceWriter{
		b:       b,
		pending: map[string]map[string][]byte{},
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// enqueue upserts records for flowID, replacing any pending upsert of the
// same connection. Never blocks the caller beyond the map update.
func (w *presenceWriter) enqueue(flowID string, records map[string][]byte) {
	if len(records) == 0 {
		return
	}
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	flow := w.pending[flowID]
	if flow == nil {
		flow = map[string][]byte{}
		w.pending[flowID] = flow
	}
	for member, record := range records {
		flow[member] = record
	}
	w.mu.Unlock()
	w.signal()
}

// cancelPending drops a connection's pending upsert.
func (w *presenceWriter) cancelPending(flowID, connID string) {
	w.mu.Lock()
	if flow := w.pending[flowID]; flow != nil {
		delete(flow, connID)
		if len(flow) == 0 {
			delete(w.pending, flowID)
		}
	}
	w.mu.Unlock()
}

// delete removes a connection's record from the flow's shared presence —
// ordered after every upsert enqueued before it — and waits (bounded) until
// the removal has run, so the caller's subsequent store reads and the leave
// it is part of see the record gone. On timeout the removal is still queued;
// only a store down for the whole bound leaves the record behind (its TTL
// reaps it).
func (w *presenceWriter) delete(flowID, connID string, wait time.Duration) {
	w.mu.Lock()
	if w.stopped {
		// The writer is gone; remove inline as a best effort (the backplane
		// applies its own round-trip timeout).
		w.mu.Unlock()
		w.b.removePresence(flowID, connID)
		return
	}
	op := deleteOp{flowID: flowID, connID: connID, done: make(chan struct{})}
	w.deletes = append(w.deletes, op)
	w.mu.Unlock()
	w.signal()
	select {
	case <-op.done:
	case <-time.After(wait):
	}
}

func (w *presenceWriter) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// run drains until stopped: swap the pending set, write every flow's records
// in one round trip each, then run the queued deletes (so a delete lands
// after the upserts enqueued before it), repeat until nothing is left, then
// block on the next wake.
func (w *presenceWriter) run() {
	defer close(w.done)
	for {
		select {
		case <-w.stop:
			return
		case <-w.wake:
		}
		for {
			w.mu.Lock()
			if len(w.pending) == 0 && len(w.deletes) == 0 {
				w.mu.Unlock()
				break
			}
			pending := w.pending
			w.pending = map[string]map[string][]byte{}
			deletes := w.deletes
			w.deletes = nil
			w.mu.Unlock()

			for flowID, records := range pending {
				w.b.writePresence(flowID, records)
			}
			for _, op := range deletes {
				w.b.removePresence(op.flowID, op.connID)
				close(op.done)
			}
		}
	}
}

// close stops the writer. Pending upserts and deletes are dropped, not
// drained: presence records are TTL-limited state the next heartbeat
// rewrites, and Shutdown's budget must not wait on store round trips for
// state that dies with the process. Waiters on a dropped delete are released.
func (w *presenceWriter) close() {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
	w.mu.Lock()
	w.stopped = true
	deletes := w.deletes
	w.deletes = nil
	w.mu.Unlock()
	for _, op := range deletes {
		close(op.done)
	}
}
