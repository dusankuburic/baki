package service

// R-streaming-3: a panic in the emit path must not leak the coalescer's emit lock: the stream settles as
// interrupted, leaves the active set, and Shutdown's wait returns.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// panicOnFirstChunk is a notifier whose first chat chunk delivery panics.
type panicOnFirstChunk struct {
	*testutil.CountingNotifier
	panicked atomic.Bool
}

func (n *panicOnFirstChunk) EmitTo(scope, name string, data interface{}) {
	if env, ok := data.(map[string]interface{}); ok && env["type"] == "chunk" && n.panicked.CompareAndSwap(false, true) {
		panic("delivery blew up")
	}
	n.CountingNotifier.EmitTo(scope, name, data)
}

func TestChatStream_PanicInEmitDoesNotHangCleanup(t *testing.T) {
	p := newParkedProvider(false)
	p.unpark()
	svc, _ := shutdownTestService(t, "emit-panic", p)
	n := &panicOnFirstChunk{CountingNotifier: &testutil.CountingNotifier{}}
	svc.notifier = n

	id, err := svc.StreamChatMessage(context.Background(), "u1", &models.FlowDocument{ID: "f-panic", Name: "f"}, nil,
		models.ChatRequest{Provider: "emit-panic", UserMessage: "hi", ClientStreamID: uuid.NewString()})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, ok := svc.finishedStreams.Load(id)
		if ok {
			ctl := v.(*streamCtl)
			select {
			case <-ctl.settled:
			case <-time.After(5 * time.Second):
				t.Fatal("the stream never settled")
			}
			ctl.mu.Lock()
			code := ctl.errCode
			ctl.mu.Unlock()
			if code != ChatErrInterrupted {
				t.Errorf("stream code %q, want %s", code, ChatErrInterrupted)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream never finished: its cleanup is blocked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, active := svc.activeStreams.Load(id); active {
		t.Error("the stream still holds its active slot")
	}
	done := make(chan struct{})
	go func() { svc.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workers.Wait never returned")
	}
}
