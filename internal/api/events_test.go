package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
