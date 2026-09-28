package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pad-core/models"
)

// TestHandleEvents_EmitsHeartbeat verifies the SSE handler writes comment-frame
// pings on an otherwise-quiet connection, so the frontend's read-inactivity
// watchdog can distinguish "no events" from a dead socket.
func TestHandleEvents_EmitsHeartbeat(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	m.heartbeatInterval = 10 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(m.HandleEvents))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want %q", got, "no")
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", got, "text/event-stream")
	}

	// No event is ever emitted; only heartbeats can produce output.
	scanner := bufio.NewScanner(resp.Body)
	pings := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), ": ping") {
			pings++
			if pings >= 3 {
				return
			}
		}
	}
	t.Fatalf("expected ≥3 heartbeat frames before the read deadline, got %d (scan err: %v)", pings, scanner.Err())
}

// TestHandleEvents_HeartbeatDoesNotReachListeners guards the client contract:
// heartbeat frames are SSE comments, never "data: " events, so they can't be
// mistaken for application events by the frontend parser.
func TestHandleEvents_HeartbeatDoesNotReachListeners(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	m.heartbeatInterval = 5 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(m.HandleEvents))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			t.Fatalf("heartbeat produced a data frame: %q", scanner.Text())
		}
	}
}

// TestDeliver_ShedsProgressToProtectChat: chat shares the per-client SSE
// buffer with analysis progress, which arrives in bursts of hundreds. Before
// shedding, a burst filled the buffer and the very next chat chunk was
// silently dropped — the client then detected the gap from the done event's
// chunk count and recovered with a full-buffer resume, re-rendering and
// reflowing the whole answer at the end of the stream.
func TestDeliver_ShedsProgressToProtectChat(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	ch := make(chan []byte, 12)
	m.clients[ch] = ""

	// Flood with progress. Shedding must stop it short of filling the buffer.
	for range 200 {
		m.Emit("analysis:progress", map[string]int{"pct": 1})
	}
	if len(ch) == cap(ch) {
		t.Fatalf("progress burst filled the buffer (%d/%d); nothing was shed", len(ch), cap(ch))
	}

	// A chat frame must still fit.
	before := len(ch)
	m.Emit("chat:event", map[string]string{"streamId": "s1", "type": "chunk"})
	if len(ch) != before+1 {
		t.Fatalf("chat frame was dropped: buffer went %d → %d (cap %d)", before, len(ch), cap(ch))
	}

	// And it must be the chat frame that is sitting at the back of the queue.
	var last []byte
	for len(ch) > 0 {
		last = <-ch
	}
	if !strings.Contains(string(last), "chat:event") {
		t.Fatalf("last queued frame was not the chat event: %q", string(last))
	}
}

// TestDeliver_NeverShedsNonProgress: only progress ticks are droppable. An
// event that cannot be reconstructed from a later message must still be
// enqueued while there is any room at all.
func TestDeliver_NeverShedsNonProgress(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	ch := make(chan []byte, 4)
	m.clients[ch] = ""

	for range 4 {
		m.Emit("analysis:complete", map[string]int{"n": 1})
	}
	if len(ch) != 4 {
		t.Fatalf("non-droppable events were shed: %d/%d buffered", len(ch), cap(ch))
	}
}

// TestEncodeEventFrame_EnforcesBound pins the frame-size contract at the pure
// encode level: small frames pass, oversized frames without a lightweight
// form are dropped, and an oversized flow:loaded is swapped for its summary
// (identity kept, document body gone).
func TestEncodeEventFrame_EnforcesBound(t *testing.T) {
	frame, ok := encodeEventFrame(Event{Name: "chat:event", Data: map[string]string{"type": "chunk"}})
	if !ok {
		t.Fatal("small frame was refused")
	}
	if !strings.Contains(string(frame), `"chat:event"`) {
		t.Fatalf("small frame lost its event name: %q", string(frame))
	}

	big := strings.Repeat("x", maxFrameBytes)
	if _, ok := encodeEventFrame(Event{Name: "chat:event", Data: map[string]string{"chunk": big}}); ok {
		t.Fatal("oversized frame without a lightweight form was accepted")
	}

	frame, ok = encodeEventFrame(Event{Name: "flow:loaded", Data: &models.FlowDocument{
		ID: "flow-1", Name: "Big", Source: big,
	}, TargetUser: "alice"})
	if !ok {
		t.Fatal("oversized flow:loaded was dropped instead of replaced by its summary")
	}
	if len(frame) > maxFrameBytes {
		t.Fatalf("replacement frame still exceeds the bound: %d bytes", len(frame))
	}
	if !strings.Contains(string(frame), `"id":"flow-1"`) || !strings.Contains(string(frame), `"name":"Big"`) {
		t.Fatalf("replacement frame lost the flow identity: %q", string(frame))
	}
	if strings.Contains(string(frame), "xxxxx") {
		t.Fatal("replacement frame still carries document content")
	}
}

// TestDeliver_ReplacesOversizedFlowLoaded verifies the bound end-to-end
// through deliver: a document-carrying flow:loaded reaches the client channel
// only as its small summary, and an oversized event with no lightweight form
// never occupies a buffer slot.
func TestDeliver_ReplacesOversizedFlowLoaded(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	ch := make(chan []byte, 4)
	m.clients[ch] = ""

	big := strings.Repeat("x", maxFrameBytes)
	m.Emit("flow:loaded", &models.FlowDocument{ID: "flow-1", Name: "Big", Source: big})
	if len(ch) != 1 {
		t.Fatalf("oversized flow:loaded was dropped instead of replaced: %d frames buffered", len(ch))
	}
	frame := string(<-ch)
	if !strings.Contains(frame, `"id":"flow-1"`) || strings.Contains(frame, "xxxxx") {
		t.Fatalf("client received wrong payload for oversized flow:loaded: %q", frame)
	}

	m.Emit("analysis:complete", map[string]string{"blob": big})
	if len(ch) != 0 {
		t.Fatalf("oversized non-convertible event occupied a buffer slot: %q", string(<-ch))
	}
}
