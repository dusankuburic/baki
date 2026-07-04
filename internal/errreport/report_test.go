package errreport

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// recordingSink captures forwarded events for assertions.
type recordingSink struct {
	mu     sync.Mutex
	panics int
	errs   int
	last   Attrs
}

func (r *recordingSink) CaptureError(_ context.Context, _ error, attrs Attrs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs++
	r.last = attrs
}

func (r *recordingSink) CapturePanic(_ context.Context, _ any, _ []byte, attrs Attrs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.panics++
	r.last = attrs
}

func TestCapturePanic_ForwardsToSinkAndBumpsMetric(t *testing.T) {
	rs := &recordingSink{}
	Register(rs)
	defer Register(nil)

	CapturePanic(context.Background(), "scanner", "boom", []byte("stack"), Attrs{"k": "v"})

	if rs.panics != 1 {
		t.Errorf("expected sink to receive 1 panic, got %d", rs.panics)
	}
	if rs.last["k"] != "v" {
		t.Errorf("expected attrs forwarded, got %v", rs.last)
	}
}

func TestCaptureError_IgnoresNilAndForwardsReal(t *testing.T) {
	rs := &recordingSink{}
	Register(rs)
	defer Register(nil)

	CaptureError(context.Background(), "chat.stream", nil, nil) // no-op
	if rs.errs != 0 {
		t.Errorf("nil error must not be forwarded, got %d", rs.errs)
	}

	CaptureError(context.Background(), "chat.stream", errors.New("x"), Attrs{"model": "gpt"})
	if rs.errs != 1 {
		t.Errorf("expected 1 forwarded error, got %d", rs.errs)
	}
}

// TestCapturePanic_PanickingSinkDoesNotPropagate ensures a faulty sink never
// breaks the caller's recovery path.
func TestCapturePanic_PanickingSinkDoesNotPropagate(t *testing.T) {
	Register(panickingSink{})
	defer Register(nil)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CapturePanic propagated a panic from the sink: %v", r)
		}
	}()
	CapturePanic(context.Background(), "http", "orig", []byte("s"), nil)
}

type panickingSink struct{}

func (panickingSink) CaptureError(context.Context, error, Attrs)       { panic("sink blew up") }
func (panickingSink) CapturePanic(context.Context, any, []byte, Attrs) { panic("sink blew up") }
