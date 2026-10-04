package api

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// openUserStream opens an SSE stream on srv as userID (the test clientKey reads
// the X-Test-User header) and returns a channel of its "data: " lines plus a
// func that closes the stream.
func openUserStream(t *testing.T, srv *httptest.Server, userID string) (<-chan string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User", userID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	lines := make(chan string, 16)
	// Closed by t.Cleanup too, so a failing test cannot leave the stream
	// open: httptest.Server.Close waits for every handler to return.
	var once sync.Once
	closeStream := func() { once.Do(func() { cancel(); resp.Body.Close() }) }
	t.Cleanup(closeStream)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				lines <- sc.Text()
			}
		}
	}()
	return lines, closeStream
}

// waitUntil polls cond for up to two seconds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEventManager_PerUserIndexFollowsConnections drives registration through
// HandleEvents: a targeted event reaches every stream of its user and no
// other, HasSubscriber answers per user, and a user whose last stream closes
// leaves no index entry behind.
func TestEventManager_PerUserIndexFollowsConnections(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	m.SetClientKeyFunc(func(r *http.Request) string { return "user:" + r.Header.Get("X-Test-User") })
	srv := httptest.NewServer(http.HandlerFunc(m.HandleEvents))
	// A cleanup, not a defer: cleanups run after the streams' own (LIFO), so
	// Close does not wait on a stream a failed assertion left open.
	t.Cleanup(srv.Close)

	a1, closeA1 := openUserStream(t, srv, "alice")
	a2, closeA2 := openUserStream(t, srv, "alice")
	b1, _ := openUserStream(t, srv, "bob")
	waitUntil(t, "three registered streams", func() bool {
		m.clientsMu.Lock()
		defer m.clientsMu.Unlock()
		return len(m.clients) == 3
	})

	m.clientsMu.Lock()
	aliceConns, bobConns := len(m.byUser["alice"]), len(m.byUser["bob"])
	m.clientsMu.Unlock()
	if aliceConns != 2 || bobConns != 1 {
		t.Fatalf("index = alice:%d bob:%d, want 2 and 1", aliceConns, bobConns)
	}
	if !m.HasSubscriber("alice") || !m.HasSubscriber("bob") || m.HasSubscriber("carol") || !m.HasSubscriber("") {
		t.Fatal("HasSubscriber disagrees with the connected streams")
	}

	m.EmitTo("alice", "chat:event", map[string]string{"n": "1"})
	for i, ch := range []<-chan string{a1, a2} {
		select {
		case line := <-ch:
			if !strings.Contains(line, "chat:event") {
				t.Fatalf("alice stream %d got %q", i, line)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("alice stream %d never received her event", i)
		}
	}
	select {
	case line := <-b1:
		t.Fatalf("bob received alice's event: %q", line)
	case <-time.After(50 * time.Millisecond):
	}

	closeA1()
	closeA2()
	waitUntil(t, "alice's streams to unregister", func() bool { return !m.HasSubscriber("alice") })
	m.clientsMu.Lock()
	_, stale := m.byUser["alice"]
	m.clientsMu.Unlock()
	if stale {
		t.Fatal("a user with no streams left kept an index entry")
	}
	if !m.HasSubscriber("bob") {
		t.Fatal("bob's stream was dropped from the index")
	}
}

// TestDeliver_TargetedCostIndependentOfOtherClients: a user-targeted event
// must not walk every connected client of every tenant under clientsMu —
// one user's chat stream would cost O(all SSE clients) per chunk. With the per-user
// index the cost must not grow with the number of OTHER users' clients. The
// gap being guarded is two orders of magnitude (10k map entries per event vs
// none: ~50ms vs well under 1ms for the batch here), so the 5x-plus-5ms bound
// leaves ample room for scheduler noise and coarse timers (Windows can report
// 0 for the uncrowded batch); the minimum of several interleaved trials is
// compared to discount pauses.
func TestDeliver_TargetedCostIndependentOfOtherClients(t *testing.T) {
	const others, events, trials = 10000, 50, 7
	build := func(n int) (*EventManager, chan []byte) {
		m := NewEventManager(make(chan struct{}))
		ch := make(chan []byte, events)
		m.clientsMu.Lock()
		m.addClientLocked(ch, "alice")
		for i := range n {
			m.addClientLocked(make(chan []byte, 1), fmt.Sprintf("user-%d", i))
		}
		m.clientsMu.Unlock()
		return m, ch
	}
	run := func(m *EventManager, ch chan []byte) time.Duration {
		start := time.Now()
		for range events {
			m.EmitTo("alice", "chat:event", "x")
		}
		d := time.Since(start)
		if len(ch) != events {
			t.Fatalf("alice received %d/%d events", len(ch), events)
		}
		for len(ch) > 0 {
			<-ch
		}
		return d
	}
	alone, aloneCh := build(0)
	crowded, crowdedCh := build(others)
	minAlone, minCrowded := time.Duration(1<<62), time.Duration(1<<62)
	for range trials {
		minAlone = min(minAlone, run(alone, aloneCh))
		minCrowded = min(minCrowded, run(crowded, crowdedCh))
	}
	if minCrowded > 5*minAlone+5*time.Millisecond {
		t.Fatalf("targeted delivery scales with other users' clients: %v with %d others vs %v alone",
			minCrowded, others, minAlone)
	}
}
