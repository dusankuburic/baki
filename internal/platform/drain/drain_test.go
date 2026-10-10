package drain

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Begin is one-way and reports only the first call; a fresh state is not draining.
func TestState_BeginOnce(t *testing.T) {
	s := New()
	if s.Draining() {
		t.Fatal("a new state is draining")
	}
	if !s.Begin() {
		t.Error("the first Begin reported false")
	}
	if s.Begin() {
		t.Error("a second Begin reported true")
	}
	if !s.Draining() {
		t.Error("not draining after Begin")
	}
	if Process().Draining() {
		t.Error("beginning a state of its own drained the process")
	}
}

// A nil state is the process's: a handler built without one follows the server's stop.
func TestState_NilIsTheProcess(t *testing.T) {
	var s *State
	if s.Draining() != Process().Draining() {
		t.Error("a nil state does not read the process's")
	}
}

// Drain begins draining before it pauses, pauses for delay, then runs the end steps in order and joins their
// errors; a cancelled ctx cuts the pause short but still runs the steps (they bound themselves on ctx).
func TestState_DrainOrder(t *testing.T) {
	s := New()
	var order []string
	errA := errors.New("a failed")
	start := time.Now()
	err := s.Drain(context.Background(), 30*time.Millisecond,
		func(context.Context) error {
			if !s.Draining() {
				t.Error("a step ran before the state was draining")
			}
			order = append(order, "a")
			return errA
		},
		func(context.Context) error { order = append(order, "b"); return nil },
	)
	if time.Since(start) < 30*time.Millisecond {
		t.Error("Drain did not pause for the delay")
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Errorf("steps ran as %v, want [a b]", order)
	}
	if !errors.Is(err, errA) {
		t.Errorf("Drain = %v, want the step's error", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	start = time.Now()
	if err := New().Drain(ctx, time.Hour, func(context.Context) error { ran = true; return nil }); err != nil {
		t.Errorf("Drain = %v", err)
	}
	if time.Since(start) > time.Second || !ran {
		t.Errorf("a cancelled ctx: paused %v, step ran %v; want no pause and the step run", time.Since(start), ran)
	}
}

// The refusal is the wire contract clients act on (ADR 0024): 503, Retry-After 2, the connection closed, and the
// standard envelope with code SERVER_RESTARTING, details.retryAfterSeconds 2 (what the web client reads) and the
// request id.
func TestRefuse_WireContract(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "req-1")
	Refuse(w)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After %q, want 2", got)
	}
	if got := w.Header().Get("Connection"); got != "close" {
		t.Errorf("Connection %q, want close", got)
	}
	var body struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"requestId"`
		Details   struct {
			RetryAfterSeconds int `json:"retryAfterSeconds"`
		} `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	if body.Code != "SERVER_RESTARTING" || body.Message == "" || body.RequestID != "req-1" || body.Details.RetryAfterSeconds != 2 {
		t.Errorf("body %+v", body)
	}
}
