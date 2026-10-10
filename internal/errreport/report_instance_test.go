package errreport

import (
	"context"
	"errors"
	"maps"
	"testing"
)

// TestForward_AddsInstanceID: every event forwarded to the sink names the replica that reported it, so a sink
// grouping the events of several replicas can tell them apart. The call site's attributes are kept, and its map is
// not written to (call sites may share one).
func TestForward_AddsInstanceID(t *testing.T) {
	rs := &recordingSink{}
	Register(rs)
	defer Register(nil)
	SetInstanceID("replica-7")
	defer SetInstanceID("")

	attrs := Attrs{"model": "gpt"}
	CaptureError(context.Background(), "chat.stream", errors.New("x"), attrs)
	if want := (Attrs{"model": "gpt", "instance_id": "replica-7"}); !maps.Equal(rs.last, want) {
		t.Errorf("error attrs = %v, want %v", rs.last, want)
	}
	CapturePanic(context.Background(), "scanner", "boom", []byte("stack"), nil)
	if want := (Attrs{"instance_id": "replica-7"}); !maps.Equal(rs.last, want) {
		t.Errorf("panic attrs = %v, want %v", rs.last, want)
	}
	if len(attrs) != 1 {
		t.Errorf("the call site's attrs were written to: %v", attrs)
	}

	// A call site's own instance_id is its context, and stays.
	CaptureError(context.Background(), "chat.stream", errors.New("x"), Attrs{"instance_id": "other"})
	if rs.last["instance_id"] != "other" {
		t.Errorf("instance_id = %v, want the call site's", rs.last["instance_id"])
	}

	// Without an instance id the attrs go through as they are.
	SetInstanceID("")
	CaptureError(context.Background(), "chat.stream", errors.New("x"), attrs)
	if !maps.Equal(rs.last, attrs) {
		t.Errorf("attrs without an instance id = %v, want %v", rs.last, attrs)
	}
}
