package api

import (
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/audit"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// auditCapture is the channel captureAuditEvents installed for the running test, nil when none: every
// testAuditRecorder sends its rows there while it is set, else straight to its backend.
var auditCapture atomic.Pointer[chan *storageif.AuditEvent]

// captureAuditEvents sends every audit row the test's handlers record, in order, to a fresh channel of size n
// (a row that finds it full goes to the log fallback); the previous capture comes back at cleanup. Handlers built
// before the call are captured too.
func captureAuditEvents(t *testing.T, n int) chan *storageif.AuditEvent {
	t.Helper()
	ch := make(chan *storageif.AuditEvent, n)
	prev := auditCapture.Swap(&ch)
	t.Cleanup(func() { auditCapture.Store(prev) })
	return ch
}

// testAuditRecorder is the audit recorder of a handler a test builds over backend: never started, so a row goes
// to the channel captureAuditEvents installed, else is written straight to backend. Without a backend it records
// nothing.
func testAuditRecorder(backend storageif.StorageBackend) *audit.Recorder {
	if backend == nil {
		return nil
	}
	return audit.NewWithSink(backend, func() chan<- *storageif.AuditEvent {
		if ch := auditCapture.Load(); ch != nil {
			return *ch
		}
		return nil
	})
}

// nextAuditEvent returns the next row on ch, failing the test after 2 s without one.
func nextAuditEvent(t *testing.T, ch chan *storageif.AuditEvent) *storageif.AuditEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no audit event was recorded")
		return nil
	}
}
