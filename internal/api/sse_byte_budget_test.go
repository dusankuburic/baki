package api

import (
	"bytes"
	"sync/atomic"
	"testing"
)

// TestOfferFrame_ClientByteBudgetBoundsQueuedFrames: the per-client channel
// bounds frame COUNT only (512 slots), so a stalled client queueing legal
// maxFrameBytes-class frames could pin 512 x 256 KiB = 128 MiB. The byte
// budget caps the queue's BYTES; exceeding it behaves like a full buffer
// (the frame is dropped), and draining refunds the budget.
func TestOfferFrame_ClientByteBudgetBoundsQueuedFrames(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	ch := make(chan []byte, 512)
	m.clientsMu.Lock()
	m.addClientLocked(ch, "alice")
	m.clientsMu.Unlock()

	frame := bytes.Repeat([]byte("x"), 1<<20) // 1 MiB legal frame
	offered := 0
	for i := 0; i < 16; i++ {
		if !m.offerFrame(ch, frame, false) {
			break
		}
		offered++
	}
	if want := int(maxClientBufferBytes >> 20); offered != want {
		t.Fatalf("queued %d MiB frames before the drop, want the %d MiB budget", offered, want)
	}
	<-ch
	// The SSE writer loop refunds the budget as frames reach the socket; do
	// the same for the frame just drained.
	if v, ok := m.bufBytes.Load(ch); ok {
		v.(*atomic.Int64).Add(-int64(len(frame)))
	}
	if !m.offerFrame(ch, frame, false) {
		t.Fatal("draining a frame must refund the byte budget")
	}
}
