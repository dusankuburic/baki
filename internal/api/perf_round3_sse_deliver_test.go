package api

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// dropWarnProbe is the default slog handler for the duration of the test:
// it counts the "send buffer full" warnings and, for each, probes whether
// the EventManager's client lock was held while the warning was written (a
// TryLock that fails means deliver logged under the lock).
type dropWarnProbe struct {
	m *EventManager

	mu            sync.Mutex
	warns         int
	heldDuringLog int
}

func (p *dropWarnProbe) Enabled(context.Context, slog.Level) bool { return true }

func (p *dropWarnProbe) Handle(_ context.Context, r slog.Record) error {
	if !strings.Contains(r.Message, "send buffer full") {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warns++
	if p.m.clientsMu.TryLock() {
		p.m.clientsMu.Unlock()
	} else {
		p.heldDuringLog++
	}
	return nil
}

func (p *dropWarnProbe) WithAttrs([]slog.Attr) slog.Handler { return p }
func (p *dropWarnProbe) WithGroup(string) slog.Handler      { return p }

// TestDeliver_DropWarningOutsideLockAndRateLimited: a stalled client (full
// channel) used to cost one synchronous log write per delivered event, inside
// the lock every other delivery and every connect/disconnect waits on. Now
// the drops are counted under the lock and reported after it is released, at
// most once per dropWarnInterval.
func TestDeliver_DropWarningOutsideLockAndRateLimited(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	probe := &dropWarnProbe{m: m}
	prev := slog.Default()
	slog.SetDefault(slog.New(probe))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A client that stopped reading: its one-slot buffer is already full, so
	// every further frame is dropped.
	stalled := make(chan []byte, 1)
	stalled <- []byte("data: x\n\n")
	m.clientsMu.Lock()
	m.addClientLocked(stalled, "alice")
	m.clientsMu.Unlock()

	const targeted, broadcast = 30, 20
	for range targeted {
		m.EmitTo("alice", "chat:chunk", "x")
	}
	for range broadcast {
		m.Emit("flow:changed", "x")
	}

	probe.mu.Lock()
	warns, held := probe.warns, probe.heldDuringLog
	probe.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d of %d drop warnings were written while clientsMu was held", held, warns)
	}
	if got := m.droppedFrames.Load(); got != targeted+broadcast {
		t.Fatalf("droppedFrames = %d, want %d (every frame to the stalled client)", got, targeted+broadcast)
	}
	if warns != 1 {
		t.Fatalf("%d drop warnings for %d drops within one dropWarnInterval; want exactly 1", warns, targeted+broadcast)
	}
}

// TestDeliver_ReadersDoNotSerialise: deliveries hold the read lock only, so a
// delivery in progress does not block another one — a connect (write lock)
// waits for both, but two fan-outs to different users proceed together.
func TestDeliver_ReadersDoNotSerialise(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	a := make(chan []byte, 4)
	b := make(chan []byte, 4)
	m.clientsMu.Lock()
	m.addClientLocked(a, "alice")
	m.addClientLocked(b, "bob")
	m.clientsMu.Unlock()

	// Hold the read lock as a delivery would, then deliver to bob from the
	// test goroutine: with a plain Mutex this would deadlock.
	m.rlockIndexed()
	done := make(chan struct{})
	go func() {
		m.EmitTo("bob", "chat:chunk", "x")
		close(done)
	}()
	<-done
	m.clientsMu.RUnlock()
	if len(b) != 1 || len(a) != 0 {
		t.Fatalf("bob got %d frames, alice %d; want 1 and 0", len(b), len(a))
	}
}
