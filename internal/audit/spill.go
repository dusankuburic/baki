package audit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"pad-analyzer/internal/metrics"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

const (
	defaultSpillMaxBytes = 10 * 1024 * 1024 // 10 MB
	spillFileName        = "audit-spill.jsonl"
)

// spillStore is a bounded, on-disk FIFO of audit events that overflowed the in-memory pool. The reaper drains
// it back into the pool when capacity returns, so a transient DB/throughput hiccup doesn't drop events.
//
// The store is a single append-only JSON-lines file guarded by a mutex. Drain rewrites the file without the
// consumed prefix — O(n) per drain, but the file is size-capped (default 10 MB) and drains are throttled, so it
// is cheap relative to the DB write it precedes.
//
// Durability trade-off: an event accepted by Spill survives a pool outage until the reaper replays it. Events
// still in the file at process exit remain on disk and drain on the next boot's Start WHEN the spill dir is on
// persistent storage; the default temp-dir location trades that cross-restart durability for zero-config
// (operators wanting it set PAD_AUDIT_SPILL_DIR to a mounted volume).
type spillStore struct {
	path     string
	maxBytes int64
	mu       sync.Mutex
	// depth is the count of events currently on the spill file, maintained under mu (cheap) so the
	// pad_audit_spill_depth gauge is accurate without a file read on every observation.
	depth int
}

func newSpillStore(dir string, maxBytes int64) (*spillStore, error) {
	if maxBytes <= 0 {
		maxBytes = defaultSpillMaxBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, spillFileName)
	// Ensure the file exists so HasItems/Stat don't error before the first spill.
	if _, err := os.Stat(p); os.IsNotExist(err) {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- path is operator-configured, not user input
		if err != nil {
			return nil, err
		}
		_ = f.Close()
	}
	return &spillStore{path: p, maxBytes: maxBytes}, nil
}

// Spill appends an event to the spill file. When the file is at capacity the event is NOT stored (the caller
// diverts it to the log fallback) and pad_audit_spill_dropped_total is bumped. Returns whether the event was
// stored.
func (s *spillStore) Spill(e *storageif.AuditEvent) bool {
	line, err := json.Marshal(e)
	if err != nil {
		return false
	}
	line = append(line, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, statErr := os.Stat(s.path); statErr == nil {
		if info.Size()+int64(len(line)) > s.maxBytes {
			metrics.RecordAuditSpillDropped()
			return false
		}
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- path is operator-configured, not user input
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return false
	}
	s.depth++
	metrics.SetAuditSpillDepth(s.depth)
	metrics.RecordAuditSpilled()
	return true
}

// Drain removes and returns up to max events from the head of the spill file, oldest first. Malformed lines
// are skipped (a corrupt line can't be replayed).
func (s *spillStore) Drain(max int) []*storageif.AuditEvent {
	if max <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil || len(data) == 0 {
		return nil
	}
	var out []*storageif.AuditEvent
	remaining := data
	for len(out) < max {
		nl := bytes.IndexByte(remaining, '\n')
		if nl < 0 {
			break
		}
		line := remaining[:nl]
		remaining = remaining[nl+1:]
		if len(line) == 0 {
			continue
		}
		var e storageif.AuditEvent
		if err := json.Unmarshal(line, &e); err == nil {
			out = append(out, &e)
		}
	}
	// Rewrite the file with the unconsumed tail ATOMICALLY (temp + rename): a plain WriteFile truncates first,
	// so a mid-write failure would lose the unconsumed tail. With rename semantics, any failure leaves the
	// ORIGINAL file (including the events counted in `out`) fully intact — we report nothing drained and let
	// the reaper retry, rather than risk replaying duplicates or corrupting the queue.
	tmp := s.path + ".drain.tmp"
	// #nosec G703 -- s.path is the operator-configured spill file
	// (PAD_AUDIT_SPILL_DIR, or a temp dir), fixed at boot. No request data
	// reaches it; the only appended component is this constant suffix.
	if err := os.WriteFile(tmp, remaining, 0o600); err != nil {
		logger.Warn("audit spill: drain rewrite failed; leaving spill file intact",
			"path", s.path, "err", err)
		return nil
	}
	if err := os.Rename(tmp, s.path); err != nil {
		logger.Warn("audit spill: drain rename failed; leaving spill file intact",
			"path", s.path, "err", err)
		_ = os.Remove(tmp)
		return nil
	}
	s.depth -= len(out)
	if s.depth < 0 {
		s.depth = 0 // defensive: file was mutated out-of-band
	}
	metrics.SetAuditSpillDepth(s.depth)
	return out
}

// HasItems reports whether the spill file holds any events.
func (s *spillStore) HasItems() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.path)
	return err == nil && info.Size() > 0
}

// reSpill re-appends already-drained events back to the spill file WITHOUT bumping pad_audit_spilled_total:
// these events were already counted when first spilled, and this is a replay retrial (the pool filled
// mid-batch), not a new overflow. Used by the reaper when it can't push a whole drained batch.
func (s *spillStore) reSpill(events []*storageif.AuditEvent) {
	if len(events) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- path is operator-configured, not user input
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		line = append(line, '\n')
		_, _ = f.Write(line)
	}
	s.depth += len(events)
	metrics.SetAuditSpillDepth(s.depth)
}
