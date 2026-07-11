package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// fakeAuditWriter records SaveAuditEvent calls and can be made to fail.
type fakeAuditWriter struct {
	mu        sync.Mutex
	saved     []*storageif.AuditEvent
	err       error // returned for every call when non-nil
	failFirst atomic.Int32
	calls     atomic.Int32
}

func (f *fakeAuditWriter) SaveAuditEvent(_ context.Context, e *storageif.AuditEvent) error {
	n := f.calls.Add(1)
	// failFirst > 0 ⇒ the first call fails, subsequent ones succeed (tests retry).
	if int32(f.failFirst.Load()) > 0 && n == 1 {
		return f.err
	}
	if f.err != nil && int32(f.failFirst.Load()) == 0 {
		return f.err
	}
	f.mu.Lock()
	f.saved = append(f.saved, e)
	f.mu.Unlock()
	return nil
}

func (f *fakeAuditWriter) count() int { return int(f.calls.Load()) }

// fakeBatchSink implements auditBatchSink; records the batch and can fail.
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
	cp := make([]*storageif.AuditEvent, len(events))
	copy(cp, events)
	f.mu.Lock()
	f.batch = append(f.batch, cp...)
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

// writeAuditBatch must use the batch sink in one call when available, and never
// touch SaveAuditEvent.
func TestWriteAuditBatch_UsesBatchSink(t *testing.T) {
	w := &fakeAuditWriter{}
	s := &fakeBatchSink{}
	writeAuditBatch(w, s, events(3))

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

// Without a batch sink, events are written one-by-one.
func TestWriteAuditBatch_PerEventWhenNoSink(t *testing.T) {
	w := &fakeAuditWriter{}
	writeAuditBatch(w, nil, events(3))

	if w.count() != 3 {
		t.Errorf("SaveAuditEvent calls = %d, want 3", w.count())
	}
}

// A failing batch falls back to per-event writes so one bad batch doesn't lose
// the whole window.
func TestWriteAuditBatch_BatchFailsFallsBackToPerEvent(t *testing.T) {
	w := &fakeAuditWriter{}
	s := &fakeBatchSink{err: errors.New("batch insert failed")}
	writeAuditBatch(w, s, events(2))

	if s.calls.Load() == 0 {
		t.Error("expected the batch sink to be attempted first")
	}
	// Per-event fallback path taken after batch fails.
	if w.count() != 2 {
		t.Errorf("per-event SaveAuditEvent calls = %d, want 2 (fallback)", w.count())
	}
}

// A transient single failure is recovered by the retry (no metric bump).
func TestWriteOneAuditEvent_RetryRecovers(t *testing.T) {
	w := &fakeAuditWriter{err: errors.New("transient")}
	w.failFirst.Store(1) // first SaveAuditEvent fails, retry succeeds

	before := metrics.AuditDroppedCount("write_failed")
	writeOneAuditEvent(w, events(1)[0])

	if w.count() != 2 {
		t.Errorf("expected 2 attempts (1 fail + 1 retry), got %d", w.count())
	}
	if n := metrics.AuditDroppedCount("write_failed") - before; n != 0 {
		t.Errorf("drop metric incremented by %v, want 0 (retry recovered)", n)
	}
}

// A persistent failure diverts to the fallback sink and bumps the metric.
func TestWriteOneAuditEvent_PersistentFailureBumpsMetric(t *testing.T) {
	w := &fakeAuditWriter{err: errors.New("db down")} // every call fails

	before := metrics.AuditDroppedCount("write_failed")
	writeOneAuditEvent(w, events(1)[0])

	if w.count() != 2 {
		t.Errorf("expected 2 attempts (initial + 1 retry), got %d", w.count())
	}
	if n := metrics.AuditDroppedCount("write_failed") - before; n != 1 {
		t.Errorf("drop metric incremented by %v, want 1 (persistent failure)", n)
	}
}

// Avoid unused-import noise if time isn't otherwise referenced in this file's
// compiled test binary path (time is used implicitly via retry delays).
var _ = time.Second

// panicAuditWriter panics on every SaveAuditEvent, simulating a driver-level
// fault inside the write path.
type panicAuditWriter struct {
	calls atomic.Int32
}

func (p *panicAuditWriter) SaveAuditEvent(_ context.Context, _ *storageif.AuditEvent) error {
	p.calls.Add(1)
	panic("driver boom")
}

// salvageAuditBatch must isolate every event with its own panic guard: none are
// lost, each is diverted to the log fallback sink (metered as salvage_failed),
// and the salvage loop itself returns normally instead of double-panicking.
func TestSalvageAuditBatch_PanicIsolation(t *testing.T) {
	w := &panicAuditWriter{}
	before := metrics.AuditDroppedCount("salvage_failed")

	salvageAuditBatch(w, events(3))

	if w.calls.Load() != 3 {
		t.Errorf("SaveAuditEvent attempted %d times, want 3 (each event isolated)", w.calls.Load())
	}
	if n := metrics.AuditDroppedCount("salvage_failed") - before; n != 3 {
		t.Errorf("salvage_failed metric incremented by %v, want 3 (one per event)", n)
	}
}

// The fallback sink writes to container logs, which are far more broadly
// readable than the audit table — raw email/IP/meta must never appear there.
func TestAuditFallback_Redaction(t *testing.T) {
	for in, want := range map[string]string{
		"dusan@example.com": "d***@example.com",
		"a@b.co":            "a***@b.co",
		"no-at-sign":        "***",
		"@lead.com":         "***",
		"":                  "",
	} {
		if got := redactEmail(in); got != want {
			t.Errorf("redactEmail(%q) = %q, want %q", in, got, want)
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
