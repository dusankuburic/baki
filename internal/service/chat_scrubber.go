package service

import (
	"strings"
	"sync"
	"time"

	"pad-core/ai/scrubber"
)

// chunkBatchInterval caps how long the chunk coalescer holds small deltas
// before flushing them as one merged "chunk" event. Each SSE event carries
// ~115 bytes of framing, so token-at-a-time providers spend most of the wire
// budget on framing; batching for one frame (~16ms) cuts that by an order of
// magnitude on fast streams. The first chunk of a stream and every terminal /
// non-chunk event (done/error/tool) bypass the batch, so first-token latency
// and completion ordering are unaffected.
const chunkBatchInterval = 16 * time.Millisecond

// scrubbedEmitter forwards model output to the client with secrets masked.
// Each text delta passes through a StreamScrubber — so a secret split across
// chunk boundaries is still caught — before being appended to the resume
// buffer and emitted as a chunk event. flush must be called at stream end
// (done or error) so text still held by the scrubber is delivered. The model's
// raw output never reaches the client or the resume buffer unscrubbed. (The
// emitted-chunk count for the done event's dropped-chunk detection lives on
// chunkCoalescer, which is the one that actually emits over SSE.)
type scrubbedEmitter struct {
	ctl   *streamCtl
	emit  func(string, map[string]interface{})
	scrub *scrubber.StreamScrubber
}

func newScrubbedEmitter(ctl *streamCtl, emit func(string, map[string]interface{})) *scrubbedEmitter {
	return &scrubbedEmitter{ctl: ctl, emit: emit, scrub: scrubber.NewStreamScrubber()}
}

func (e *scrubbedEmitter) text(t string) { e.push(e.scrub.Write(t)) }
func (e *scrubbedEmitter) flush()        { e.push(e.scrub.Flush()) }

func (e *scrubbedEmitter) push(t string) {
	if t == "" {
		return
	}
	e.ctl.mu.Lock()
	e.ctl.buffer.WriteString(t)
	e.ctl.mu.Unlock()
	e.emit("chunk", map[string]interface{}{"content": t})
}

// chunkCoalescer batches consecutive "chunk" events to cut SSE framing
// overhead on fast token streams. Each SSE event carries ~115 bytes of framing
// regardless of payload size; a token-at-a-time provider emitting 5-char deltas
// spends ~95% of the wire budget on framing. The coalescer merges deltas for up
// to chunkBatchInterval before forwarding a single merged "chunk" event.
//
// Ordering guarantees:
//   - The FIRST chunk of a stream is emitted immediately so the user sees the
//     first token without a frame of added latency.
//   - Non-chunk events (done/error/tool) flush the pending batch FIRST, then
//     pass through, so chunks always precede the terminal event in order.
//   - flush is goroutine-safe: the batch timer fires from time.AfterFunc's
//     goroutine while the worker thread drives flush via non-chunk events.
//
// The emitted-chunk count (count) replaces scrubbedEmitter.chunks in the done
// event's dropped-chunk field so the client's received-vs-expected check stays
// meaningful (it now counts EMITTED events, which is what the client observes).
type chunkCoalescer struct {
	emit  func(string, map[string]interface{}) // raw notifier emit (NOT the wrapped one)
	mu    sync.Mutex
	buf   strings.Builder
	timer *time.Timer
	first bool
	count int // emitted chunk events (for done's dropped-chunk detection)
}

func newChunkCoalescer(emit func(string, map[string]interface{})) *chunkCoalescer {
	return &chunkCoalescer{emit: emit, first: true}
}

// wrap returns an emit-shaped function that routes "chunk" through the batch
// and flushes before any other event type. Bind this as the worker's emit so
// every event (scrubbedEmitter + direct done/error/tool) routes through here.
func (c *chunkCoalescer) wrap() func(string, map[string]interface{}) {
	return func(eventType string, data map[string]interface{}) {
		if eventType != "chunk" {
			c.flush()
			c.emit(eventType, data)
			return
		}
		content, _ := data["content"].(string)
		if content == "" {
			return
		}
		c.add(content)
	}
}

// add appends a delta to the batch, or emits immediately for the stream's first
// delta (first-token latency protection).
func (c *chunkCoalescer) add(content string) {
	c.mu.Lock()
	if c.first {
		c.first = false
		c.count++
		c.mu.Unlock()
		// Emit OUTSIDE the lock: c.emit reaches EventManager.deliver which
		// takes clientsMu and iterates every connected SSE client. Holding
		// chunkCoalescer.mu across it couples the batch critical section to
		// every client and is a latency hazard (no deadlock today only because
		// nothing takes clientsMu then reaches this mutex). content is a Go
		// string (immutable), so it's safe to use after unlocking.
		c.emit("chunk", map[string]interface{}{"content": content})
		return
	}
	c.buf.WriteString(content)
	if c.timer == nil {
		c.timer = time.AfterFunc(chunkBatchInterval, c.flush)
	}
	c.mu.Unlock()
}

// flush emits any pending batch. Safe for concurrent use (timer goroutine +
// worker). A no-op when nothing is pending.
func (c *chunkCoalescer) flush() {
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.buf.Len() == 0 {
		c.mu.Unlock()
		return
	}
	// Snapshot the content under the lock, then emit outside it (see add).
	content := c.buf.String()
	c.buf.Reset()
	c.count++
	c.mu.Unlock()
	c.emit("chunk", map[string]interface{}{"content": content})
}

// flushAndCount flushes the pending batch and returns the total emitted-chunk
// count, for the done event's dropped-chunk detection field.
func (c *chunkCoalescer) flushAndCount() int {
	c.flush()
	c.mu.Lock()
	n := c.count
	c.mu.Unlock()
	return n
}
