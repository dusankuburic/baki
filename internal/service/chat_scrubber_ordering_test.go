package service

import (
	"sync"
	"testing"
	"time"
)

// TestChunkCoalescer_TerminalNeverPrecedesChunk is the regression test for the
// SSE event-reordering fix. A batched (non-first) chunk arms a ~16ms timer; the
// timer goroutine and the worker's terminal ("done") emit then race. Before the
// fix, flush() released the batch mutex before emitting, so the timer's chunk
// could be delivered AFTER the worker's done — reordering the terminal event
// ahead of the final chunk. With emitMu serializing emits, done is always
// emitted last. Runs many rounds around the batch interval, under `go test -race`.
func TestChunkCoalescer_TerminalNeverPrecedesChunk(t *testing.T) {
	for round := 0; round < 100; round++ {
		rec := &emitRecorder{}
		c := newChunkCoalescer(rec.emit)
		emit := c.wrap()

		emit("chunk", map[string]interface{}{"content": "first"}) // immediate
		emit("chunk", map[string]interface{}{"content": "tail"})  // batched → arms the timer

		// Fire the terminal event concurrently with the batch timer so the two
		// emit paths race. Sleeping ~the batch interval lines the timer's flush
		// up with the done emit.
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(chunkBatchInterval)
			emit("done", map[string]interface{}{})
		}()
		wg.Wait()

		rec.mu.Lock()
		calls := append([]struct{ typ, content string }(nil), rec.calls...)
		rec.mu.Unlock()

		doneIdx := -1
		for i, ca := range calls {
			if ca.typ == "done" {
				doneIdx = i
			}
		}
		if doneIdx == -1 {
			t.Fatalf("round %d: no done event recorded: %+v", round, calls)
		}
		if doneIdx != len(calls)-1 {
			t.Fatalf("round %d: a chunk was emitted AFTER done (reordering): %+v", round, calls)
		}
	}
}
