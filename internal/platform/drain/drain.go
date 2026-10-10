// Package drain is the process's draining state, the first step of the server's stop (ADR 0024). Once it begins,
// the readiness probe answers 503 at once, and new chat streams, event streams and WebSocket upgrades are refused
// with 503 SERVER_RESTARTING and a Retry-After, so clients go to another replica or come back once this one has
// restarted. The work already running is then ended over the connections that are still open, and only after that
// does the server close them.
package drain

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"pad-core/logger"
)

const (
	// Code is the machine code of a refusal while draining, and of the terminal chat event of a stream the
	// shutdown ended: the client may retry, once, instead of reporting a failure.
	Code = "SERVER_RESTARTING"
	// RetryAfter is the Retry-After of a refusal: about when another replica, or this server restarted, answers.
	RetryAfter = 2 * time.Second
	// refusalMessage is the refusal's text, for a client that does not know Code.
	refusalMessage = "the server is restarting — try again in a moment"
)

// State is a draining flag: set once, never cleared, since a process drains only on its way out. A nil *State
// stands for the process's own (Process), so a handler built without one follows the server's stop.
type State struct {
	draining atomic.Bool
}

var process State

// Process is the process's draining state: what the server's stop hook begins and every handler consults, unless
// a test gives the handler a state of its own (New).
func Process() *State { return &process }

// New returns a state of its own, not draining. Tests use one, so beginning it does not drain the whole binary.
func New() *State { return &State{} }

func (s *State) or() *State {
	if s == nil {
		return &process
	}
	return s
}

// Begin starts draining and reports whether this call did (false when it already was).
func (s *State) Begin() bool {
	if !s.or().draining.CompareAndSwap(false, true) {
		return false
	}
	logger.Info("server draining")
	return true
}

// Draining reports whether Begin was called.
func (s *State) Draining() bool { return s.or().draining.Load() }

// Drain is the start of the server's stop: Begin, a pause of delay so that the readiness probe and load balancers
// see the 503 while requests still flow, then each of end in order, the work that must finish while the clients'
// connections are still open (ChatService.Shutdown delivers the chat streams' terminal events). ctx bounds it all;
// the caller closes the connections once Drain returns. The errors of end are joined.
func (s *State) Drain(ctx context.Context, delay time.Duration, end ...func(context.Context) error) error {
	s.Begin()
	if delay > 0 {
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
	}
	var errs []error
	for _, fn := range end {
		if err := fn(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Refuse answers a request that would start long-lived work while the server drains: 503 with the standard error
// envelope (code SERVER_RESTARTING; the message is not masked, it says what happened), the wait both as Retry-After
// and as details.retryAfterSeconds (the web client cannot read response headers), and "Connection: close" so the
// retry opens a connection a load balancer may route to another replica. Nothing was started: the retry is safe.
func Refuse(w http.ResponseWriter) {
	seconds := int(RetryAfter / time.Second)
	h := w.Header()
	h.Set("Retry-After", strconv.Itoa(seconds))
	h.Set("Connection", "close")
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	// The fields of render.ErrorResponse, which this package cannot import.
	_ = json.NewEncoder(w).Encode(struct {
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		RequestID string         `json:"requestId,omitempty"`
		Details   map[string]any `json:"details"`
	}{Code: Code, Message: refusalMessage, RequestID: h.Get("X-Request-ID"), Details: map[string]any{"retryAfterSeconds": seconds}})
}
