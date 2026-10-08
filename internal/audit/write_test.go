package audit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/metrics"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// fakeWriter records SaveAuditEvent calls and can be made to fail.
type fakeWriter struct {
	mu        sync.Mutex
	saved     []*storageif.AuditEvent
	err       error // returned for every call when non-nil
	failFirst atomic.Int32
	calls     atomic.Int32
}

func (f *fakeWriter) SaveAuditEvent(_ context.Context, e *storageif.AuditEvent) error {
	n := f.calls.Add(1)
	// failFirst > 0 ⇒ the first call fails, subsequent ones succeed (tests retry).
	if f.failFirst.Load() > 0 && n == 1 {
		return f.err
	}
	if f.err != nil && f.failFirst.Load() == 0 {
		return f.err
	}
	f.mu.Lock()
	f.saved = append(f.saved, e)
	f.mu.Unlock()
	return nil
}

func (f *fakeWriter) SaveAuditEvents(ctx context.Context, events []*storageif.AuditEvent) error {
	for _, e := range events {
		if err := f.SaveAuditEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeWriter) ListAuditEvents(context.Context, storageif.AuditFilter) ([]*storageif.AuditEvent, error) {
	return nil, nil
}

func (f *fakeWriter) count() int { return int(f.calls.Load()) }

func (f *fakeWriter) savedEvents() []*storageif.AuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*storageif.AuditEvent(nil), f.saved...)
}

// fakeBatchSink implements batchSink; records the batch and can fail.
type fakeBatchSink struct {
	mu    sync.Mutex
	batch []*storageif.AuditEvent
	err   error
	calls atomic.Int32
}

func (f *fakeBatchSink) SaveAuditEvents(_ context.Context, events []*storageif.AuditEvent) error {
	f.calls.Add(1)
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	f.batch = append(f.batch, events...)
	f.mu.Unlock()
	return nil
}

func events(n int) []*storageif.AuditEvent {
	out := make([]*storageif.AuditEvent, n)
	for i := range out {
		out[i] = &storageif.AuditEvent{ID: string(rune('a' + i)), Action: "test"}
	}
	return out
}

// writeBatch must use the batch sink in one call when available, and never touch SaveAuditEvent.
func TestWriteBatch_UsesBatchSink(t *testing.T) {
	w := &fakeWriter{}
	s := &fakeBatchSink{}
	writeBatch(w, s, events(3))

	if s.calls.Load() != 1 {
		t.Errorf("batch sink calls = %d, want 1", s.calls.Load())
	}
	if w.count() != 0 {
		t.Errorf("per-event SaveAuditEvent called %d times, want 0 (batch succeeded)", w.count())
	}
	if len(s.batch) != 3 {
		t.Errorf("batch persisted %d events, want 3", len(s.batch))
	}
}

// A failing batch falls back to per-event writes so one bad batch doesn't lose the whole window.
func TestWriteBatch_BatchFailsFallsBackToPerEvent(t *testing.T) {
	w := &fakeWriter{}
	s := &fakeBatchSink{err: errors.New("batch insert failed")}
	writeBatch(w, s, events(2))

	if s.calls.Load() == 0 {
		t.Error("expected the batch sink to be attempted first")
	}
	if w.count() != 2 {
		t.Errorf("per-event SaveAuditEvent calls = %d, want 2 (fallback)", w.count())
	}
}

// A transient single failure is recovered by the retry (no metric bump).
func TestWriteOne_RetryRecovers(t *testing.T) {
	w := &fakeWriter{err: errors.New("transient")}
	w.failFirst.Store(1) // first SaveAuditEvent fails, retry succeeds

	before := metrics.AuditDroppedCount("write_failed")
	writeOne(w, events(1)[0])

	if w.count() != 2 {
		t.Errorf("expected 2 attempts (1 fail + 1 retry), got %d", w.count())
	}
	if n := metrics.AuditDroppedCount("write_failed") - before; n != 0 {
		t.Errorf("drop metric incremented by %v, want 0 (retry recovered)", n)
	}
}

// A persistent failure diverts to the fallback sink and bumps the metric.
func TestWriteOne_PersistentFailureBumpsMetric(t *testing.T) {
	w := &fakeWriter{err: errors.New("db down")} // every call fails

	before := metrics.AuditDroppedCount("write_failed")
	writeOne(w, events(1)[0])

	if w.count() != 2 {
		t.Errorf("expected 2 attempts (initial + 1 retry), got %d", w.count())
	}
	if n := metrics.AuditDroppedCount("write_failed") - before; n != 1 {
		t.Errorf("drop metric incremented by %v, want 1 (persistent failure)", n)
	}
}

// panicWriter panics on every SaveAuditEvent, simulating a driver-level fault inside the write path.
type panicWriter struct {
	calls atomic.Int32
}

func (p *panicWriter) SaveAuditEvent(context.Context, *storageif.AuditEvent) error {
	p.calls.Add(1)
	panic("driver boom")
}

// salvageBatch must isolate every event with its own panic guard: none are lost, each is diverted to the log
// fallback sink (metered as salvage_failed), and the salvage loop itself returns normally instead of
// double-panicking.
func TestSalvageBatch_PanicIsolation(t *testing.T) {
	w := &panicWriter{}
	before := metrics.AuditDroppedCount("salvage_failed")

	salvageBatch(w, events(3))

	if w.calls.Load() != 3 {
		t.Errorf("SaveAuditEvent attempted %d times, want 3 (each event isolated)", w.calls.Load())
	}
	if n := metrics.AuditDroppedCount("salvage_failed") - before; n != 3 {
		t.Errorf("salvage_failed metric incremented by %v, want 3 (one per event)", n)
	}
}

// The fallback sink writes to container logs, which are far more broadly readable than the audit table — raw
// email/IP/meta must never appear there.
func TestFallback_Redaction(t *testing.T) {
	for in, want := range map[string]string{
		"dusan@example.com": "d***@example.com",
		"a@b.co":            "a***@b.co",
		"no-at-sign":        "***",
		"@lead.com":         "***",
		"":                  "",
	} {
		if got := RedactEmail(in); got != want {
			t.Errorf("RedactEmail(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"203.0.113.7": "203.0.x.x",
		"10.1.2.3":    "10.1.x.x",
		"2001:db8::1": "2001:…",
		"not-an-ip":   "***",
		"":            "",
	} {
		if got := redactIP(in); got != want {
			t.Errorf("redactIP(%q) = %q, want %q", in, got, want)
		}
	}
	keys := metaKeys(map[string]string{"target_email": "x@y.z", "role": "admin"})
	if len(keys) != 2 || keys[0] != "role" || keys[1] != "target_email" {
		t.Errorf("metaKeys = %v, want sorted [role target_email] (keys only, no values)", keys)
	}
	if metaKeys(nil) != nil {
		t.Error("metaKeys(nil) should be nil")
	}
}
